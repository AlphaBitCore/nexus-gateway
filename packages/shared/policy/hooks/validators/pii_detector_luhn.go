package validators

// luhnValid checks a numeric string (ignoring spaces and hyphens) with the Luhn algorithm.
func luhnValid(s string) bool {
	// Pre-allocate for typical card number length (16 digits + separators).
	digits := make([]int, 0, 20)
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			digits = append(digits, int(ch-'0'))
		}
	}
	if len(digits) == 0 {
		return false
	}

	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}
