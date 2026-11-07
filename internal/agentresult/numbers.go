package agentresult

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

const maxNumberBytes = 1 << 20

// numbersForValidator проверяет точность и явно переводит числа для валидатора.
func numbersForValidator(v any) (any, error) {
	// numbersForValidator проверяет точность и явно переводит числа для валидатора.
	type frame struct {
		src   any
		depth int
		set   func(any)
	}
	var result any
	stack := []frame{{src: v, depth: 0, set: func(x any) { result = x }}}
	for len(stack) > 0 {
		i := len(stack) - 1
		f := stack[i]
		stack = stack[:i]
		if f.depth > 10000 {
			return nil, errors.New("number")
		}
		switch x := f.src.(type) {
		case json.Number:
			n, e := numberFloat(x)
			if e != nil {
				return nil, e
			}
			f.set(n)
		case []any:
			a := make([]any, len(x))
			f.set(a)
			for j := len(x) - 1; j >= 0; j-- {
				jj := j
				stack = append(stack, frame{src: x[j], depth: f.depth + 1, set: func(v any) { a[jj] = v }})
			}
		case map[string]any:
			m := make(map[string]any, len(x))
			f.set(m)
			for k, v := range x {
				key, val := k, v
				stack = append(stack, frame{src: val, depth: f.depth + 1, set: func(v any) { m[key] = v }})
			}
		default:
			f.set(f.src)
		}
	}
	return result, nil
}

func numberFloat(n json.Number) (float64, error) {
	s := string(n)
	if len(s) == 0 || len(s) > maxNumberBytes {
		return 0, errors.New("number")
	}
	if mantissaZero(s) {
		return 0, nil
	}
	f, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsInf(f, 0) || math.IsNaN(f) || f == 0 && !isDecimalZero(s) {
		return 0, errors.New("number")
	}
	want, e := normalizeDecimal(s)
	if e != nil {
		return 0, e
	}
	got, e := normalizeDecimal(strconv.FormatFloat(f, 'g', -1, 64))
	if e != nil || want.text != got.text {
		return 0, errors.New("number")
	}
	if want.integer { // IEEE-754 точно представляет целые числа только в диапазоне до 2^53-1.
		if f > 9007199254740991 || f < -9007199254740991 {
			return 0, errors.New("number")
		}
	}
	return f, nil
}

type decimalForm struct {
	text    string
	integer bool
}

func mantissaZero(s string) bool {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	for _, c := range s {
		if c >= '1' && c <= '9' {
			return false
		}
	}
	return true
}

func isDecimalZero(s string) bool {
	return mantissaZero(s)
}

// normalizeDecimal ограничивает показатель и приводит десятичную запись к канонической форме.
func normalizeDecimal(s string) (decimalForm, error) {
	if mantissaZero(s) {
		return decimalForm{text: "0", integer: true}, nil
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	parts := strings.SplitN(s, "e", 2)
	if len(parts) == 1 {
		parts = strings.SplitN(s, "E", 2)
	}
	mant := parts[0]
	exp := int64(0)
	if len(parts) == 2 {
		t := parts[1]
		sign := int64(1)
		if strings.HasPrefix(t, "+") {
			t = t[1:]
		} else if strings.HasPrefix(t, "-") {
			sign = -1
			t = t[1:]
		}
		t = strings.TrimLeft(t, "0")
		if t == "" {
			t = "0"
		}
		if len(t) > 7 {
			return decimalForm{}, errors.New("number")
		}
		x, e := strconv.ParseInt(t, 10, 32)
		if e != nil {
			return decimalForm{}, errors.New("number")
		}
		exp = sign * x
	}
	p := strings.SplitN(mant, ".", 2)
	whole := p[0]
	frac := ""
	if len(p) == 2 {
		frac = p[1]
	}
	digits := whole + frac
	for len(digits) > 0 && digits[0] == '0' {
		digits = digits[1:]
	}
	if digits == "" {
		return decimalForm{text: "0", integer: true}, nil
	}
	exp -= int64(len(frac))
	for len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		exp++
	}
	if exp > 2000000 || exp < -2000000 {
		return decimalForm{}, errors.New("number")
	}
	// Каноническая научная запись сопоставляет равные десятичные значения.
	power := exp + int64(len(digits)) - 1
	prefix := ""
	if neg {
		prefix = "-"
	}
	text := prefix + digits[:1]
	if len(digits) > 1 {
		text += "." + digits[1:]
	}
	text += "e" + strconv.FormatInt(power, 10)
	integer := exp >= 0
	return decimalForm{text, integer}, nil
}
