package irmaclient

import (
	"math/big"
	"os"
	"testing"

	"github.com/mhe/gabi"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/fs"
	"github.com/privacybydesign/irmago/internal/test"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	test.ClearTestStorage(nil)
	test.CreateTestStorage(nil)
	retCode := m.Run()
	test.ClearTestStorage(nil)
	os.Exit(retCode)
}

type IgnoringClientHandler struct{}

func (i *IgnoringClientHandler) UpdateConfiguration(new *irma.IrmaIdentifierSet)                 {}
func (i *IgnoringClientHandler) UpdateAttributes()                                               {}
func (i *IgnoringClientHandler) EnrollmentError(manager irma.SchemeManagerIdentifier, err error) {}
func (i *IgnoringClientHandler) EnrollmentSuccess(manager irma.SchemeManagerIdentifier)          {}

func parseStorage(t *testing.T) *Client {
	require.NoError(t, fs.CopyDirectory("../testdata/teststorage", "../testdata/storage/test"))
	manager, err := New(
		"../testdata/storage/test",
		"../testdata/irma_configuration",
		"",
		&IgnoringClientHandler{},
	)
	require.NoError(t, err)
	return manager
}

func verifyClientIsUnmarshaled(t *testing.T, client *Client) {
	cred, err := client.credential(irma.NewCredentialTypeIdentifier("irma-demo.RU.studentCard"), 0)
	require.NoError(t, err, "could not fetch credential")
	require.NotNil(t, cred, "Credential should exist")
	require.NotNil(t, cred.Attributes[0], "Metadata attribute of irma-demo.RU.studentCard should not be nil")

	cred, err = client.credential(irma.NewCredentialTypeIdentifier("test.test.mijnirma"), 0)
	require.NoError(t, err, "could not fetch credential")
	require.NotNil(t, cred, "Credential should exist")
	require.NotNil(t, cred.Signature.KeyshareP)

	require.NotEmpty(t, client.CredentialInfoList())

	pk, err := cred.PublicKey()
	require.NoError(t, err)
	require.True(t,
		cred.Signature.Verify(pk, cred.Attributes),
		"Credential should be valid",
	)
}

func verifyCredentials(t *testing.T, client *Client) {
	var pk *gabi.PublicKey
	var err error
	for credtype, credsmap := range client.credentials {
		for index, cred := range credsmap {
			pk, err = cred.PublicKey()
			require.NoError(t, err)
			require.True(t,
				cred.Credential.Signature.Verify(pk, cred.Attributes),
				"Credential %s-%d was invalid", credtype.String(), index,
			)
			require.Equal(t, cred.Attributes[0], client.secretkey.Key,
				"Secret key of credential %s-%d unequal to main secret key",
				cred.CredentialType().Identifier().String(), index,
			)
		}
	}
}

func verifyPaillierKey(t *testing.T, PrivateKey *paillierPrivateKey) {
	require.NotNil(t, PrivateKey)
	require.NotNil(t, PrivateKey.L)
	require.NotNil(t, PrivateKey.U)
	require.NotNil(t, PrivateKey.PublicKey.N)

	require.Equal(t, big.NewInt(1), new(big.Int).Exp(big.NewInt(2), PrivateKey.L, PrivateKey.N))
	require.Equal(t, PrivateKey.NSquared, new(big.Int).Exp(PrivateKey.N, big.NewInt(2), nil))

	plaintext := "Hello Paillier!"
	ciphertext, err := PrivateKey.Encrypt([]byte(plaintext))
	require.NoError(t, err)
	decrypted, err := PrivateKey.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, plaintext, string(decrypted))
}

func verifyKeyshareIsUnmarshaled(t *testing.T, client *Client) {
	require.NotNil(t, client.paillierKeyCache)
	require.NotNil(t, client.keyshareServers)
	testManager := irma.NewSchemeManagerIdentifier("test")
	require.Contains(t, client.keyshareServers, testManager)
	kss := client.keyshareServers[testManager]
	require.NotEmpty(t, kss.Nonce)

	verifyPaillierKey(t, kss.PrivateKey)
	verifyPaillierKey(t, client.paillierKeyCache)
}

func TestStorageDeserialization(t *testing.T) {
	client := parseStorage(t)
	verifyClientIsUnmarshaled(t, client)
	verifyCredentials(t, client)
	verifyKeyshareIsUnmarshaled(t, client)

	test.ClearTestStorage(t)
}

func TestLogging(t *testing.T) {
	client := parseStorage(t)

	logs, err := client.Logs()
	oldLogLength := len(logs)
	require.NoError(t, err)

	// Do session so we can examine its log item later
	jwt := getCombinedJwt("testip", irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"))
	sessionHelper(t, jwt, "issue", client)

	logs, err = client.Logs()
	require.NoError(t, err)
	require.True(t, len(logs) == oldLogLength+1)

	entry := logs[len(logs)-1]
	require.NotNil(t, entry)
	sessionjwt, err := entry.Jwt()
	require.NoError(t, err)
	require.Equal(t, "testip", sessionjwt.(*irma.IdentityProviderJwt).ServerName)
	require.NoError(t, err)
	require.NotEmpty(t, entry.Disclosed)
	require.NotEmpty(t, entry.Received)
	response, err := entry.GetResponse()
	require.NoError(t, err)
	require.NotNil(t, response)
	require.IsType(t, &gabi.IssueCommitmentMessage{}, response)

	test.ClearTestStorage(t)
}

func TestCandidates(t *testing.T) {
	client := parseStorage(t)

	attrtype := irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
	disjunction := &irma.AttributeDisjunction{
		Attributes: []irma.AttributeTypeIdentifier{attrtype},
	}
	attrs := client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Len(t, attrs, 1)

	attr := attrs[0]
	require.NotNil(t, attr)
	require.Equal(t, attr.Type, attrtype)

	disjunction = &irma.AttributeDisjunction{
		Attributes: []irma.AttributeTypeIdentifier{attrtype},
		Values:     map[irma.AttributeTypeIdentifier]string{attrtype: "456"},
	}
	attrs = client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Len(t, attrs, 1)

	disjunction = &irma.AttributeDisjunction{
		Attributes: []irma.AttributeTypeIdentifier{attrtype},
		Values:     map[irma.AttributeTypeIdentifier]string{attrtype: "foobarbaz"},
	}
	attrs = client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Empty(t, attrs)

	test.ClearTestStorage(t)
}

func TestPaillier(t *testing.T) {
	client := parseStorage(t)

	challenge, _ := gabi.RandomBigInt(256)
	comm, _ := gabi.RandomBigInt(1000)
	resp, _ := gabi.RandomBigInt(1000)

	sk := client.paillierKey(true)
	bytes, err := sk.Encrypt(challenge.Bytes())
	require.NoError(t, err)
	cipher := new(big.Int).SetBytes(bytes)

	bytes, err = sk.Encrypt(comm.Bytes())
	require.NoError(t, err)
	commcipher := new(big.Int).SetBytes(bytes)

	// [[ c ]]^resp * [[ comm ]]
	cipher.Exp(cipher, resp, sk.NSquared).Mul(cipher, commcipher).Mod(cipher, sk.NSquared)

	bytes, err = sk.Decrypt(cipher.Bytes())
	require.NoError(t, err)
	plaintext := new(big.Int).SetBytes(bytes)
	expected := new(big.Int).Set(challenge)
	expected.Mul(expected, resp).Add(expected, comm)

	require.Equal(t, plaintext, expected)

	test.ClearTestStorage(t)
}

func TestCredentialRemoval(t *testing.T) {
	client := parseStorage(t)

	id := irma.NewCredentialTypeIdentifier("irma-demo.RU.studentCard")
	id2 := irma.NewCredentialTypeIdentifier("test.test.mijnirma")

	cred, err := client.credential(id, 0)
	require.NoError(t, err)
	require.NotNil(t, cred)
	err = client.RemoveCredentialByHash(cred.AttributeList().Hash())
	require.NoError(t, err)
	cred, err = client.credential(id, 0)
	require.NoError(t, err)
	require.Nil(t, cred)

	cred, err = client.credential(id2, 0)
	require.NoError(t, err)
	require.NotNil(t, cred)
	err = client.RemoveCredential(id2, 0)
	require.NoError(t, err)
	cred, err = client.credential(id2, 0)
	require.NoError(t, err)
	require.Nil(t, cred)

	test.ClearTestStorage(t)
}

func TestWrongSchemeManager(t *testing.T) {
	client := parseStorage(t)

	irmademo := irma.NewSchemeManagerIdentifier("irma-demo")
	require.Contains(t, client.Configuration.SchemeManagers, irmademo)
	require.NoError(t, os.Remove("../testdata/storage/test/irma_configuration/irma-demo/index"))

	err := client.Configuration.ParseFolder()
	_, ok := err.(*irma.SchemeManagerError)
	require.True(t, ok)
	require.Contains(t, client.Configuration.DisabledSchemeManagers, irmademo)
	require.Contains(t, client.Configuration.SchemeManagers, irmademo)
	require.NotEqual(t,
		client.Configuration.SchemeManagers[irmademo].Status,
		irma.SchemeManagerStatusValid,
	)

	test.ClearTestStorage(t)
}

// Test installing a new scheme manager from a qr, and do a(n issuance) session
// within this manager to test the autmatic downloading of credential definitions,
// issuers, and public keys.
func TestDownloadSchemeManager(t *testing.T) {
	client := parseStorage(t)

	// Remove irma-demo scheme manager as we need to test adding it
	irmademo := irma.NewSchemeManagerIdentifier("irma-demo")
	require.Contains(t, client.Configuration.SchemeManagers, irmademo)
	require.NoError(t, client.Configuration.RemoveSchemeManager(irmademo, true))
	require.NotContains(t, client.Configuration.SchemeManagers, irmademo)

	// Do an add-scheme-manager-session
	qr := &irma.Qr{
		Type: irma.ActionSchemeManager,
		URL:  "https://raw.githubusercontent.com/credentials/irma-demo-schememanager/master",
	}
	c := make(chan *irma.SessionError)
	client.NewSession(qr, TestHandler{t, c, client})
	if err := <-c; err != nil {
		t.Fatal(*err)
	}
	require.Contains(t, client.Configuration.SchemeManagers, irmademo)

	// Do a session to test downloading of cred types, issuers and keys
	jwt := getCombinedJwt("testip", irma.NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"))
	sessionHelper(t, jwt, "issue", client)

	require.Contains(t, client.Configuration.SchemeManagers, irmademo)
	require.Contains(t, client.Configuration.Issuers, irma.NewIssuerIdentifier("irma-demo.RU"))
	require.Contains(t, client.Configuration.CredentialTypes, irma.NewCredentialTypeIdentifier("irma-demo.RU.studentCard"))

	basepath := "../testdata/storage/test/irma_configuration/irma-demo"
	exists, err := fs.PathExists(basepath + "/description.xml")
	require.NoError(t, err)
	require.True(t, exists)
	exists, err = fs.PathExists(basepath + "/RU/description.xml")
	require.NoError(t, err)
	require.True(t, exists)
	exists, err = fs.PathExists(basepath + "/RU/Issues/studentCard/description.xml")
	require.NoError(t, err)
	require.True(t, exists)

	test.ClearTestStorage(t)
}
