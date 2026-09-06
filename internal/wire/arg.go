package wire

// ARG forms: the low nibble of the first byte of a token, or a full
// byte in bare argument positions inside record bodies.
const (
	argInlineMax uint64 = 11 // inline values 0..11
	argU8        byte   = 0x0C
	argU16       byte   = 0x0D
	argU32       byte   = 0x0E
	argU64       byte   = 0x0F
	// argExt is the extended form: selector 10 ‖ ARG n
	// (n ≥ 9) ‖ n big-endian bytes, for magnitudes beyond the u64 form.
	// Legal only where the grammar routes it — BIGINT value bodies. In
	// every token position the low nibble tops out at 0xF, and bare
	// argument positions reject it as an unknown form.
	argExt byte = 0x10
)

// extArgMinLen is the least legal ext-ARG byte count: 9 BE bytes carry
// bit lengths 65..72, all ≥ 2^64 — anything shorter fits a u64 form and
// would be non-minimal.
const extArgMinLen = 9

// zigzag maps a signed int64 onto unsigned values ordered by magnitude:
// 0→0, -1→1, 1→2, -2→3, MinInt64→MaxUint64.
func zigzag(n int64) uint64 {
	return uint64(n<<1) ^ uint64(n>>63)
}

func unzigzag(u uint64) int64 {
	return int64(u>>1) ^ -int64(u&1)
}

// ArgForm returns the minimal ARG selector for n — the public form for
// codec-layer order reasoning over ARG-prefixed token bytes.
func ArgForm(n uint64) byte { return argForm(n) }

// argForm returns the minimal ARG selector for n.
func argForm(n uint64) byte {
	switch {
	case n <= argInlineMax:
		return byte(n)
	case n <= 0xFF:
		return argU8
	case n <= 0xFFFF:
		return argU16
	case n <= 0xFFFFFFFF:
		return argU32
	default:
		return argU64
	}
}

// appendArgBytes appends the big-endian payload of n for the given form;
// inline forms carry the value in the selector byte itself.
func appendArgBytes(dst []byte, form byte, n uint64) []byte {
	switch form {
	case argU8:
		return append(dst, byte(n))
	case argU16:
		return append(dst, byte(n>>8), byte(n))
	case argU32:
		return append(dst, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	case argU64:
		return append(dst, byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default: // inline: no payload bytes
		return dst
	}
}

// writeArg writes a bare argument: inline selector or width selector followed
// by big-endian bytes (minimal form only).
func (w *Writer) writeArg(n uint64) {
	form := argForm(n)
	w.buf = append(w.buf, form)
	w.buf = appendArgBytes(w.buf, form, n)
}

// writeTokenArg writes a token first byte carrying an ARG: class in the high
// nibble, minimal ARG form in the low nibble, followed by the payload.
func (w *Writer) writeTokenArg(class byte, n uint64) {
	form := argForm(n)
	w.buf = append(w.buf, class<<4|form)
	w.buf = appendArgBytes(w.buf, form, n)
}

// argPayload decodes the payload for an ARG form, enforcing minimality:
// a width form carrying a value that fits a narrower form is
// ErrFormat.
func (r *Reader) argPayload(form byte) (uint64, error) {
	switch {
	case form <= byte(argInlineMax):
		return uint64(form), nil
	case form == argU8:
		b, err := r.byteAt()
		if err != nil {
			return 0, err
		}
		if b <= byte(argInlineMax) {
			return 0, werr(kindMalformedArg, "wire: non-minimal u8 argument %d", b)
		}
		return uint64(b), nil
	case form == argU16:
		b, err := r.readN(2)
		if err != nil {
			return 0, err
		}
		v := uint64(b[0])<<8 | uint64(b[1])
		if v <= 0xFF {
			return 0, werr(kindMalformedArg, "wire: non-minimal u16 argument %d", v)
		}
		return v, nil
	case form == argU32:
		b, err := r.readN(4)
		if err != nil {
			return 0, err
		}
		v := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
		if v <= 0xFFFF {
			return 0, werr(kindMalformedArg, "wire: non-minimal u32 argument %d", v)
		}
		return v, nil
	case form == argU64:
		b, err := r.readN(8)
		if err != nil {
			return 0, err
		}
		v := uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
			uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
		if v <= 0xFFFFFFFF {
			return 0, werr(kindMalformedArg, "wire: non-minimal u64 argument %d", v)
		}
		return v, nil
	default:
		return 0, werr(kindMalformedOp, "wire: unknown argument form 0x%02X", form)
	}
}

// writeExtArg writes the extended argument for the big-endian magnitude
// b: selector, minimal ARG length, raw bytes. The
// caller guarantees len(b) ≥ 9 and b[0] ≠ 0.
func (w *Writer) writeExtArg(b []byte) {
	w.buf = append(w.buf, argExt)
	w.writeArg(uint64(len(b)))
	w.buf = append(w.buf, b...)
}

// readExtArg reads the extended argument: selector,
// ARG length, big-endian bytes. Minimality is enforced — n < 9 or a zero
// leading byte is ErrFormat; an advertised n above maxLen is ErrBudget
// before the body is consumed (non-zero maxLen gates, decode budgets).
func (r *Reader) readExtArg(maxLen uint64) ([]byte, error) {
	n, err := r.ReadArg()
	if err != nil {
		return nil, err
	}
	if n < extArgMinLen {
		return nil, werr(kindMalformedArg, "wire: non-minimal ext argument length %d", n)
	}
	if maxLen != 0 && n > maxLen {
		return nil, werr(kindBudgetBytes, "wire: ext argument length %d exceeds budget %d", n, maxLen)
	}
	b, err := r.readN(n)
	if err != nil {
		return nil, err
	}
	if b[0] == 0 {
		return nil, werr(kindMalformedArg, "wire: ext argument with zero leading byte")
	}
	return b, nil
}

// ReadArg reads a bare argument from a record body.
func (r *Reader) ReadArg() (uint64, error) {
	b, err := r.byteAt()
	if err != nil {
		return 0, err
	}
	return r.argPayload(b)
}

// readFirst reads the first byte of a token and verifies its class.
func (r *Reader) readFirst(class byte) (byte, error) {
	b, err := r.byteAt()
	if err != nil {
		return 0, err
	}
	if b>>4 != class {
		return 0, werr(kindMalformedOp, "wire: expected class 0x%X, got 0x%02X", class, b)
	}
	return b & 0x0F, nil
}

// readTokenArg reads the argument of a token whose low nibble is an ARG form.
func (r *Reader) readTokenArg(class byte) (uint64, error) {
	form, err := r.readFirst(class)
	if err != nil {
		return 0, err
	}
	return r.argPayload(form)
}
