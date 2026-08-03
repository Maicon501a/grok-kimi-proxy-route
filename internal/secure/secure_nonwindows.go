//go:build !windows

package secure

func encryptAtRest(plain []byte) ([]byte, error) {
	return aesGCMSeal(plain)
}

func decryptDPAPI(_ []byte) ([]byte, error) {
	return nil, ErrCannotDecrypt
}
