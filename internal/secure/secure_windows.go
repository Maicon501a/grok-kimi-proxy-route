//go:build windows

package secure

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func encryptAtRest(plain []byte) ([]byte, error) {
	return dpapiProtect(plain)
}

func decryptDPAPI(blob []byte) ([]byte, error) {
	return dpapiUnprotect(blob)
}

// dpapiProtect encrypts data with CryptProtectData (current-user scope).
// CRYPTPROTECT_UI_FORBIDDEN prevents any prompt; the blob is persisted, not
// memory-pinned.
func dpapiProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{
		Size: uint32(len(plain)),
		Data: &plain[0],
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(
		&in,
		nil, // description
		nil, // optional entropy
		0,   // reserved
		nil, // prompt struct
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&out,
	); err != nil {
		return nil, fmt.Errorf("secure: CryptProtectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}

func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, ErrCannotDecrypt
	}
	in := windows.DataBlob{
		Size: uint32(len(blob)),
		Data: &blob[0],
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(
		&in,
		nil, // description
		nil, // optional entropy
		0,   // reserved
		nil, // prompt struct
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&out,
	); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, int(out.Size))...), nil
}
