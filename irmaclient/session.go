package irmaclient

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"math/big"

	"github.com/go-errors/errors"
	"github.com/mhe/gabi"
	"github.com/privacybydesign/irmago"
)

// This file contains the logic and state of performing IRMA sessions, communicates
// with IRMA API servers, and uses the calling Client to construct messages and replies
// in the IRMA protocol.

// PermissionHandler is a callback for providing permission for an IRMA session
// and specifying the attributes to be disclosed.
type PermissionHandler func(proceed bool, choice *irma.DisclosureChoice)

// PinHandler is used to provide the user's PIN code.
type PinHandler func(proceed bool, pin string)

// A Handler contains callbacks for communication to the user.
type Handler interface {
	StatusUpdate(action irma.Action, status irma.Status)
	Success(action irma.Action, result string)
	Cancelled(action irma.Action)
	Failure(action irma.Action, err *irma.SessionError)
	UnsatisfiableRequest(action irma.Action, ServerName string, missing irma.AttributeDisjunctionList)

	KeyshareBlocked(manager irma.SchemeManagerIdentifier, duration int)
	KeyshareEnrollmentIncomplete(manager irma.SchemeManagerIdentifier)
	KeyshareEnrollmentMissing(manager irma.SchemeManagerIdentifier)

	RequestIssuancePermission(request irma.IssuanceRequest, ServerName string, callback PermissionHandler)
	RequestVerificationPermission(request irma.DisclosureRequest, ServerName string, callback PermissionHandler)
	RequestSignaturePermission(request irma.SignatureRequest, ServerName string, callback PermissionHandler)
	RequestSchemeManagerPermission(manager *irma.SchemeManager, callback func(proceed bool))

	RequestPin(remainingAttempts int, callback PinHandler)
}

// SessionDismisser can dismiss the current IRMA session.
type SessionDismisser interface {
	Dismiss()
}

type session struct {
	Action  irma.Action
	Handler Handler
	Version irma.Version

	choice      *irma.DisclosureChoice
	client      *Client
	downloaded  *irma.IrmaIdentifierSet
	irmaSession irma.IrmaSession
	done        bool

	// These are empty on manual sessions
	ServerURL string
	info      *irma.SessionInfo
	jwt       irma.RequestorJwt
	transport *irma.HTTPTransport
}

// We implement the handler for the keyshare protocol
var _ keyshareSessionHandler = (*session)(nil)

// Supported protocol versions. Minor version numbers should be reverse sorted.
var supportedVersions = map[int][]int{
	2: {2, 1},
}

func calcVersion(qr *irma.Qr) (string, error) {
	// Parse range supported by server
	var minmajor, minminor, maxmajor, maxminor int
	var err error
	if minmajor, err = strconv.Atoi(string(qr.ProtocolVersion[0])); err != nil {
		return "", err
	}
	if minminor, err = strconv.Atoi(string(qr.ProtocolVersion[2])); err != nil {
		return "", err
	}
	if maxmajor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[0])); err != nil {
		return "", err
	}
	if maxminor, err = strconv.Atoi(string(qr.ProtocolMaxVersion[2])); err != nil {
		return "", err
	}

	// Iterate supportedVersions in reverse sorted order (i.e. biggest major number first)
	keys := make([]int, 0, len(supportedVersions))
	for k := range supportedVersions {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	for _, major := range keys {
		for _, minor := range supportedVersions[major] {
			aboveMinimum := major > minmajor || (major == minmajor && minor >= minminor)
			underMaximum := major < maxmajor || (major == maxmajor && minor <= maxminor)
			if aboveMinimum && underMaximum {
				return fmt.Sprintf("%d.%d", major, minor), nil
			}
		}
	}
	return "", fmt.Errorf("No supported protocol version between %s and %s", qr.ProtocolVersion, qr.ProtocolMaxVersion)
}

func (session *session) IsInteractive() bool {
	return session.ServerURL != ""
}

func (session *session) getBuilders() (gabi.ProofBuilderList, error) {
	var builders gabi.ProofBuilderList
	var err error

	switch session.Action {
	case irma.ActionSigning:
		fallthrough
	case irma.ActionDisclosing:
		builders, err = session.client.ProofBuilders(session.choice)
	case irma.ActionIssuing:
		builders, err = session.client.IssuanceProofBuilders(session.irmaSession.(*irma.IssuanceRequest))
	}

	return builders, err
}

func (session *session) getProof() (interface{}, error) {
	var message interface{}
	var err error

	switch session.Action {
	case irma.ActionSigning:
		message, err = session.client.Proofs(session.choice, session.irmaSession, true)
	case irma.ActionDisclosing:
		message, err = session.client.Proofs(session.choice, session.irmaSession, false)
	case irma.ActionIssuing:
		message, err = session.client.IssueCommitments(session.irmaSession.(*irma.IssuanceRequest))
	}

	return message, err
}

// checkKeyshareEnrollment checks if we are enrolled into all involved keyshare servers,
// and aborts the session if not
func (session *session) checkKeyshareEnrollment() bool {
	for id := range session.irmaSession.Identifiers().SchemeManagers {
		manager, ok := session.client.Configuration.SchemeManagers[id]
		if !ok {
			session.Handler.Failure(session.Action, &irma.SessionError{ErrorType: irma.ErrorUnknownSchemeManager, Info: id.String()})
			return false
		}
		distributed := manager.Distributed()
		_, enrolled := session.client.keyshareServers[id]
		if distributed && !enrolled {
			session.Handler.KeyshareEnrollmentMissing(id)
			return false
		}
	}
	return true
}

func (session *session) panicFailure() {
	if e := recover(); e != nil {
		if session.Handler != nil {
			session.Handler.Failure(session.Action, panicToError(e))
		}
	}
}

func (session *session) checkAndUpateConfiguration(client *Client) bool {
	var err error
	for id := range session.irmaSession.Identifiers().SchemeManagers {
		manager, contains := client.Configuration.SchemeManagers[id]
		if !contains {
			session.fail(&irma.SessionError{
				ErrorType: irma.ErrorUnknownSchemeManager,
				Info:      id.String(),
			})
			return false
		}
		if !manager.Valid {
			session.fail(&irma.SessionError{
				ErrorType: irma.ErrorInvalidSchemeManager,
				Info:      string(manager.Status),
			})
			return false
		}
	}

	// Check if we are enrolled into all involved keyshare servers
	if !session.checkKeyshareEnrollment() {
		return false
	}

	// Download missing credential types/issuers/public keys from the scheme manager
	if session.downloaded, err = session.client.Configuration.Download(session.irmaSession.Identifiers()); err != nil {
		session.fail(&irma.SessionError{ErrorType: irma.ErrorConfigurationDownload, Err: err})
		return false
	}
	return true
}

// NewManualSession starts a manual session, given a signature request in JSON and a handler to pass messages to
func (client *Client) NewManualSession(sigrequestJSONString string, handler Handler) {
	var err error
	sigrequest := &irma.SignatureRequest{}
	if err = json.Unmarshal([]byte(sigrequestJSONString), sigrequest); err != nil {
		handler.Failure(irma.ActionUnknown, &irma.SessionError{Err: err})
		return
	}

	session := &session{
		Action:      irma.ActionSigning, // TODO hardcoded for now
		Handler:     handler,
		client:      client,
		Version:     irma.Version("2"), // TODO hardcoded for now
		irmaSession: sigrequest,
	}

	session.Handler.StatusUpdate(session.Action, irma.StatusManualStarted)

	if !session.checkAndUpateConfiguration(client) {
		return
	}

	candidates, missing := session.client.CheckSatisfiability(session.irmaSession.ToDisclose())
	if len(missing) > 0 {
		session.Handler.UnsatisfiableRequest(session.Action, "E-mail request", missing)
		// TODO: session.transport.Delete() on dialog cancel
		return
	}
	session.irmaSession.SetCandidates(candidates)

	// Ask for permission to execute the session
	callback := PermissionHandler(func(proceed bool, choice *irma.DisclosureChoice) {
		session.choice = choice
		session.irmaSession.SetDisclosureChoice(choice)
		go session.do(proceed)
	})
	session.Handler.RequestSignaturePermission(
		*session.irmaSession.(*irma.SignatureRequest), "E-mail request", callback)
}

// NewSession creates and starts a new interactive IRMA session
func (client *Client) NewSession(qr *irma.Qr, handler Handler) SessionDismisser {
	session := &session{
		ServerURL: qr.URL,
		transport: irma.NewHTTPTransport(qr.URL),
		Action:    irma.Action(qr.Type),
		Handler:   handler,
		client:    client,
	}

	if session.Action == irma.ActionSchemeManager {
		go session.managerSession()
		return session
	}

	version, err := calcVersion(qr)
	if err != nil {
		session.fail(&irma.SessionError{ErrorType: irma.ErrorProtocolVersionNotSupported, Err: err})
		return nil
	}
	session.Version = irma.Version(version)

	// Check if the action is one of the supported types
	switch session.Action {
	case irma.ActionDisclosing: // nop
	case irma.ActionSigning: // nop
	case irma.ActionIssuing: // nop
	case irma.ActionUnknown:
		fallthrough
	default:
		session.fail(&irma.SessionError{ErrorType: irma.ErrorUnknownAction, Info: string(session.Action)})
		return nil
	}

	if !strings.HasSuffix(session.ServerURL, "/") {
		session.ServerURL += "/"
	}

	go session.start()

	return session
}

// start retrieves the first message in the IRMA protocol, checks if we can perform
// the request, and informs the user of the outcome.
func (session *session) start() {
	defer session.panicFailure()

	session.Handler.StatusUpdate(session.Action, irma.StatusCommunicating)

	// Get the first IRMA protocol message and parse it
	session.info = &irma.SessionInfo{}
	Err := session.transport.Get("jwt", session.info)
	if Err != nil {
		session.fail(Err.(*irma.SessionError))
		return
	}

	var err error
	session.jwt, err = irma.ParseRequestorJwt(session.Action, session.info.Jwt)
	if err != nil {
		session.fail(&irma.SessionError{ErrorType: irma.ErrorInvalidJWT, Err: err})
		return
	}
	session.irmaSession = session.jwt.IrmaSession()
	session.irmaSession.SetContext(session.info.Context)
	session.irmaSession.SetNonce(session.info.Nonce)
	if session.Action == irma.ActionIssuing {
		ir := session.irmaSession.(*irma.IssuanceRequest)
		// Store which public keys the server will use
		for _, credreq := range ir.Credentials {
			credreq.KeyCounter = session.info.Keys[credreq.CredentialTypeID.IssuerIdentifier()]
		}
	}

	if !session.checkAndUpateConfiguration(session.client) {
		return
	}

	if session.Action == irma.ActionIssuing {
		ir := session.irmaSession.(*irma.IssuanceRequest)
		for _, credreq := range ir.Credentials {
			info, err := credreq.Info(session.client.Configuration)
			if err != nil {
				session.fail(&irma.SessionError{ErrorType: irma.ErrorUnknownCredentialType, Err: err})
				return
			}
			ir.CredentialInfoList = append(ir.CredentialInfoList, info)
		}
	}

	candidates, missing := session.client.CheckSatisfiability(session.irmaSession.ToDisclose())
	if len(missing) > 0 {
		session.Handler.UnsatisfiableRequest(session.Action, session.jwt.Requestor(), missing)
		// TODO: session.transport.Delete() on dialog cancel
		return
	}
	session.irmaSession.SetCandidates(candidates)

	// Ask for permission to execute the session
	callback := PermissionHandler(func(proceed bool, choice *irma.DisclosureChoice) {
		session.choice = choice
		session.irmaSession.SetDisclosureChoice(choice)
		go session.do(proceed)
	})
	session.Handler.StatusUpdate(session.Action, irma.StatusConnected)
	switch session.Action {
	case irma.ActionDisclosing:
		session.Handler.RequestVerificationPermission(
			*session.irmaSession.(*irma.DisclosureRequest), session.jwt.Requestor(), callback)
	case irma.ActionSigning:
		session.Handler.RequestSignaturePermission(
			*session.irmaSession.(*irma.SignatureRequest), session.jwt.Requestor(), callback)
	case irma.ActionIssuing:
		session.Handler.RequestIssuancePermission(
			*session.irmaSession.(*irma.IssuanceRequest), session.jwt.Requestor(), callback)
	default:
		panic("Invalid session type") // does not happen, session.Action has been checked earlier
	}
}

func (session *session) do(proceed bool) {
	defer session.panicFailure()

	if !proceed {
		session.cancel()
		return
	}
	session.Handler.StatusUpdate(session.Action, irma.StatusCommunicating)

	if !session.irmaSession.Identifiers().Distributed(session.client.Configuration) {
		message, err := session.getProof()
		if err != nil {
			session.fail(&irma.SessionError{ErrorType: irma.ErrorCrypto, Err: err})
			return
		}
		session.sendResponse(message)
	} else {
		builders, err := session.getBuilders()
		if err != nil {
			session.fail(&irma.SessionError{ErrorType: irma.ErrorCrypto, Err: err})
		}
		startKeyshareSession(
			session,
			session.Handler,
			builders,
			session.irmaSession,
			session.client.Configuration,
			session.client.keyshareServers,
			session.client.state,
		)
	}
}

func (session *session) KeyshareDone(message interface{}) {
	session.sendResponse(message)
}

func (session *session) KeyshareCancelled() {
	session.cancel()
}

func (session *session) KeyshareEnrollmentIncomplete(manager irma.SchemeManagerIdentifier) {
	session.Handler.KeyshareEnrollmentIncomplete(manager)
}

func (session *session) KeyshareBlocked(manager irma.SchemeManagerIdentifier, duration int) {
	session.Handler.KeyshareBlocked(manager, duration)
}

func (session *session) KeyshareError(manager *irma.SchemeManagerIdentifier, err error) {
	var serr *irma.SessionError
	var ok bool
	if serr, ok = err.(*irma.SessionError); !ok {
		serr = &irma.SessionError{ErrorType: irma.ErrorKeyshare, Err: err}
	} else {
		serr.ErrorType = irma.ErrorKeyshare
	}
	session.fail(serr)
}

func (session *session) KeysharePin() {
	session.Handler.StatusUpdate(session.Action, irma.StatusConnected)
}

func (session *session) KeysharePinOK() {
	session.Handler.StatusUpdate(session.Action, irma.StatusCommunicating)
}

type disclosureResponse string

func (session *session) sendResponse(message interface{}) {
	var log *LogEntry
	var err error
	var messageJson []byte

	if session.IsInteractive() {
		switch session.Action {
		case irma.ActionSigning:
			fallthrough
		case irma.ActionDisclosing:
			var response disclosureResponse
			if err = session.transport.Post("proofs", &response, message); err != nil {
				session.fail(err.(*irma.SessionError))
				return
			}
			if response != "VALID" {
				session.fail(&irma.SessionError{ErrorType: irma.ErrorRejected, Info: string(response)})
				return
			}
			log, _ = session.createLogEntry(message.(gabi.ProofList)) // TODO err
		case irma.ActionIssuing:
			response := []*gabi.IssueSignatureMessage{}
			if err = session.transport.Post("commitments", &response, message); err != nil {
				session.fail(err.(*irma.SessionError))
				return
			}
			if err = session.client.ConstructCredentials(response, session.irmaSession.(*irma.IssuanceRequest)); err != nil {
				session.fail(&irma.SessionError{ErrorType: irma.ErrorCrypto, Err: err})
				return
			}
			log, _ = session.createLogEntry(message) // TODO err
		}
	} else {
		messageJson, err = json.Marshal(message)
		if err != nil {
			session.fail(&irma.SessionError{ErrorType: irma.ErrorSerialization, Err: err})
			return
		}
	}

	_ = session.client.addLogEntry(log) // TODO err
	if !session.downloaded.Empty() {
		session.client.handler.UpdateConfiguration(session.downloaded)
	}
	if session.Action == irma.ActionIssuing {
		session.client.handler.UpdateAttributes()
	}
	session.done = true
	session.Handler.Success(session.Action, string(messageJson))
}

func (session *session) managerSession() {
	defer func() {
		if e := recover(); e != nil {
			if session.Handler != nil {
				session.Handler.Failure(session.Action, panicToError(e))
			}
		}
	}()

	// We have to download the scheme manager description.xml here before installing it,
	// because we need to show its contents (name, description, website) to the user
	// when asking installation permission.
	manager, err := irma.DownloadSchemeManager(session.ServerURL)
	if err != nil {
		session.Handler.Failure(session.Action, &irma.SessionError{ErrorType: irma.ErrorConfigurationDownload, Err: err})
		return
	}

	session.Handler.RequestSchemeManagerPermission(manager, func(proceed bool) {
		if !proceed {
			session.Handler.Cancelled(session.Action) // No need to DELETE session here
			return
		}
		if err := session.client.Configuration.InstallSchemeManager(manager); err != nil {
			session.Handler.Failure(session.Action, &irma.SessionError{ErrorType: irma.ErrorConfigurationDownload, Err: err})
			return
		}

		// Update state and inform user of success
		if manager.Distributed() {
			session.client.UnenrolledSchemeManagers = session.client.unenrolledSchemeManagers()
		}
		session.client.handler.UpdateConfiguration(
			&irma.IrmaIdentifierSet{
				SchemeManagers:  map[irma.SchemeManagerIdentifier]struct{}{manager.Identifier(): {}},
				Issuers:         map[irma.IssuerIdentifier]struct{}{},
				CredentialTypes: map[irma.CredentialTypeIdentifier]struct{}{},
			},
		)
		session.Handler.Success(session.Action, "")
	})
	return
}

// Session lifetime functions

func panicToError(e interface{}) *irma.SessionError {
	var info string
	switch x := e.(type) {
	case string:
		info = x
	case error:
		info = x.Error()
	case fmt.Stringer:
		info = x.String()
	default: // nop
	}
	return &irma.SessionError{ErrorType: irma.ErrorPanic, Info: info}
}

// Idempotently send DELETE to remote server, returning whether or not we did something
func (session *session) delete() bool {
	if !session.done {
		if session.IsInteractive() {
			session.transport.Delete()
		}
		session.done = true
		return true
	}
	return false
}

func (session *session) fail(err *irma.SessionError) {
	if session.delete() {
		err.Err = errors.Wrap(err.Err, 0)
		if session.downloaded != nil && !session.downloaded.Empty() {
			session.client.handler.UpdateConfiguration(session.downloaded)
		}
		session.Handler.Failure(session.Action, err)
	}
}

func (session *session) cancel() {
	if session.delete() {
		if session.downloaded != nil && !session.downloaded.Empty() {
			session.client.handler.UpdateConfiguration(session.downloaded)
		}
		session.Handler.Cancelled(session.Action)
	}
}

func (session *session) Dismiss() {
	session.cancel()
}

type issuanceState struct {
	nonce2   *big.Int
	builders []*gabi.CredentialBuilder
}

func newIssuanceState() (*issuanceState, error) {
	nonce2, err := gabi.RandomBigInt(gabi.DefaultSystemParameters[4096].Lstatzk)
	if err != nil {
		return nil, err
	}
	return &issuanceState{
		nonce2:   nonce2,
		builders: []*gabi.CredentialBuilder{},
	}, nil
}
