package probe

// ValidMethod reports whether method is a non-empty RFC 9110 token.
func ValidMethod(method string) bool { return validToken(method) }

// ValidHeaderName reports whether name is a non-empty RFC 9110 token.
func ValidHeaderName(name string) bool { return validToken(name) }

// ValidHeaderValue rejects bytes net/http cannot safely place in a field value.
func ValidHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		byteValue := value[i]
		if (byteValue < 0x20 && byteValue != '\t') || byteValue == 0x7f {
			return false
		}
	}
	return true
}

func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		byteValue := value[i]
		if byteValue >= 'a' && byteValue <= 'z' ||
			byteValue >= 'A' && byteValue <= 'Z' ||
			byteValue >= '0' && byteValue <= '9' {
			continue
		}
		switch byteValue {
		case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
			continue
		default:
			return false
		}
	}
	return true
}
