package service_test

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"

	"github.com/thehappieco/mailie/internal/keyscheme"
	"github.com/thehappieco/mailie/internal/service"
)

// keyed is a link as a person's browser sends it (docs/key-scheme.md section
// 12.11): with the public half of a fresh mailbox key pair, a fresh namespace
// and a grant's shape at epoch 1, which the server checks and cannot open.
// An instance key's link is left as it is: an operator mailbox has no key.
func keyed(p service.Principal, req service.AddAccountRequest) service.AddAccountRequest {
	if !p.IsSession() {
		return req
	}
	req.PublicKey = b64(mailboxPublicKey())
	req.Namespace = keyscheme.NewSealID()
	req.Grant = b64(grantAt(1))
	return req
}

// mailboxPublicKey is the public half of a fresh X25519 key pair, as a
// browser makes a mailbox's.
func mailboxPublicKey() []byte {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return key.PublicKey().Bytes()
}

// grantAt is 88 random bytes with a grant's header at epoch: what the server
// checks of a grant, sealing nothing.
func grantAt(epoch int) []byte {
	g := make([]byte, keyscheme.GrantLen)
	//nolint:errcheck // crypto/rand.Read never returns an error
	_, _ = rand.Read(g)
	copy(g, []byte{0x4d, 0x4c, 0x01, 0x01, 0x01})
	binary.BigEndian.PutUint16(g[5:7], uint16(epoch)) //nolint:gosec // G115: tests pass epochs that fit
	g[7] = 0
	return g
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
