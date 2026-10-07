package main

import "strconv"

// chaosValue writes any JSON value: null or empty at the configured rates,
// otherwise a random scalar, array or object. Only scalars appear at the depth
// limit.
func (b *builder) chaosValue(depth int) {
	x := b.r.Float64()
	if x < b.cfg.pNull {
		b.buf = append(b.buf, "null"...)
		return
	}
	if x < b.cfg.pNull+b.cfg.pEmpty {
		switch b.r.IntN(3) {
		case 0:
			b.buf = append(b.buf, `""`...)
		case 1:
			b.buf = append(b.buf, "[]"...)
		default:
			b.buf = append(b.buf, "{}"...)
		}
		return
	}

	kind := b.r.IntN(10)
	if depth >= b.cfg.depth {
		kind = b.r.IntN(7) // scalars only at the depth limit
	}
	switch kind {
	case 0, 1, 2:
		b.strN(chaosAlphabet, b.textLen())
	case 3: // small int
		b.buf = strconv.AppendInt(b.buf, int64(b.r.IntN(100000))-1000, 10)
	case 4: // full-range int64
		b.buf = strconv.AppendInt(b.buf, int64(b.r.Uint64()), 10)
	case 5: // float, shortest round-trip repr (may use exponent form)
		b.buf = strconv.AppendFloat(b.buf, b.r.NormFloat64()*float64(int64(1)<<b.r.IntN(40)), 'g', -1, 64)
	case 6:
		b.buf = strconv.AppendBool(b.buf, b.r.IntN(2) == 0)
	case 7, 8: // array
		n := 1 + b.r.IntN(6)
		b.buf = append(b.buf, '[')
		for i := range n {
			if i > 0 {
				b.buf = append(b.buf, ',')
			}
			b.chaosValue(depth + 1)
		}
		b.buf = append(b.buf, ']')
	default: // object
		b.buf = append(b.buf, '{')
		b.key("k")
		b.chaosValue(depth + 1)
		b.someFields(depth + 1)
		b.buf = append(b.buf, '}')
	}
}
