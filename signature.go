package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/hex"
	"fmt"

	"github.com/getlantern/go-update"
)

func checksumForFile(file string) (string, []byte, error) {
	var checksum []byte
	var err error
	if checksum, err = update.ChecksumForFile(file); err != nil {
		return "", nil, err
	}
	checksumHex := hex.EncodeToString(checksum)
	return checksumHex, checksum, nil
}

func signatureForFile(file string, privKey *rsa.PrivateKey) (string, error) {
	_, checksum, err := checksumForFile(file)
	if err != nil {
		return "", err
	}

	// Checking message signature.
	signature, err := rsa.SignPKCS1v15(rand.Reader, privKey, crypto.SHA256, checksum)
	if err != nil {
		return "", fmt.Errorf("Could not create signature for file %s: %q", file, err)
	}

	return hex.EncodeToString(signature), nil
}
