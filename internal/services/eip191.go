package services

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// EIP-191 personal_sign, the signature a wallet makes over a Sign-In with
// Ethereum (EIP-4361) message: the board checks one when an agent links a
// wallet (identity.link kind wallet, RFC0015 "Raise your standing").

// PersonalSignDigest is keccak256("\x19Ethereum Signed Message:\n" +
// len(message) + message).
func PersonalSignDigest(message []byte) [32]byte {
	return keccak256([]byte("\x19Ethereum Signed Message:\n"+strconv.Itoa(len(message))), message)
}

// SignPersonal signs message with a 32-byte secp256k1 key the way a wallet's
// personal_sign does, for clients and tests: the key's address and the
// signature as 0x and 130 hex digits, r || s || v with v 27 or 28.
func SignPersonal(key []byte, message []byte) (EVMAddress, string, error) {
	k, err := signerFromKeyBytes(key)
	if err != nil {
		return EVMAddress{}, "", err
	}
	sig, err := signDigest(k.key, PersonalSignDigest(message))
	if err != nil {
		return EVMAddress{}, "", err
	}
	return k.addr, "0x" + hex.EncodeToString(sig[:]), nil
}

// RecoverPersonalSign returns the address whose key signed message with
// personal_sign. signature is 0x and 130 hex digits, r || s || v with v 27,
// 28, 0 or 1. A malformed signature or one no key produces is refused.
func RecoverPersonalSign(message []byte, signature string) (EVMAddress, bool) {
	if len(signature) != 132 || !strings.HasPrefix(signature, "0x") {
		return EVMAddress{}, false
	}
	raw, err := hex.DecodeString(signature[2:])
	if err != nil || len(raw) != 65 {
		return EVMAddress{}, false
	}
	var sig [65]byte
	copy(sig[:], raw)
	switch sig[64] {
	case 0, 1:
		sig[64] += 27
	case 27, 28:
	default:
		return EVMAddress{}, false
	}
	return recoverAddress(PersonalSignDigest(message), sig)
}
