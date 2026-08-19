package accio

const (
	securityTableSize       = 256
	securityTableMultiplier = uint32(0x343fd)
	securityTableIncrement  = uint32(0x269ec3)
)

// generateSecurityTable reproduces the per-process table recovered from the
// Windows SecurityGuardSDK runtime. The SDK seeds it with Unix seconds.
func generateSecurityTable(seed uint32) [securityTableSize]byte {
	var table [securityTableSize]byte
	state := seed
	for i := range table {
		state = state*securityTableMultiplier + securityTableIncrement
		table[i] = byte((state >> 16) & 0x7f)
	}
	return table
}
