package datum

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// MaxDecimalDigits is the largest coefficient width a decimal value may use.
// The coefficient is held in an int64, so 18 decimal digits keep every value
// comfortably inside the signed 64-bit range while bounding storage, journal
// and index cost.
const MaxDecimalDigits = 18

// MaxDecimalLiteral bounds the lexical form accepted for a decimal. A value
// that can be stored exactly needs at most 18 significant digits, a sign and a
// decimal point; the extra room lets callers send additional fractional digits
// that a column may round away, without letting an unbounded string reach the
// parser.
const MaxDecimalLiteral = 64

type Value struct {
	Kind string
	Int  int64
	Text string
	Bool bool
	Dec  Decimal
}

// Decimal is an exact base-10 value: Unscaled / 10^Scale. It never passes
// through binary floating point. Scale is kept in 0..MaxDecimalDigits and the
// coefficient magnitude is kept below 10^MaxDecimalDigits by ParseDecimal,
// NormalizeDecimal and column coercion.
//
// Raw holds an exact lexical form that has not yet been resolved against a
// column domain (a JSON number with more digits than a coefficient can hold).
// It exists only between JSON decoding and column coercion; resolved values
// always have Raw empty. Keeping it lets an unbound literal be judged by the
// same declared precision/scale/rounding as every other spelling instead of
// being rejected by the decoder alone.
type Decimal struct {
	Unscaled int64  `json:"unscaled"`
	Scale    int32  `json:"scale"`
	Raw      string `json:"-"`
}

type Rounding string

const (
	RoundReject   Rounding = "reject"
	RoundHalfEven Rounding = "half_even"
	RoundHalfUp   Rounding = "half_up"
	RoundTruncate Rounding = "truncate"
)

func ParseRounding(s string) (Rounding, error) {
	switch Rounding(s) {
	case "", RoundReject:
		return RoundReject, nil
	case RoundHalfEven, RoundHalfUp, RoundTruncate:
		return Rounding(s), nil
	default:
		return "", fmt.Errorf("unsupported rounding %q (use reject, half_even, half_up or truncate)", s)
	}
}

var powersOfTen = [...]int64{
	1,
	10,
	100,
	1000,
	10000,
	100000,
	1000000,
	10000000,
	100000000,
	1000000000,
	10000000000,
	100000000000,
	1000000000000,
	10000000000000,
	100000000000000,
	1000000000000000,
	10000000000000000,
	100000000000000000,
	1000000000000000000,
}

// Pow10 returns 10^n for 0 <= n <= 18.
func Pow10(n int) int64 {
	if n < 0 || n >= len(powersOfTen) {
		return 0
	}
	return powersOfTen[n]
}

func (d Decimal) Valid() bool {
	return d.Raw == "" && d.Scale >= 0 && d.Scale <= MaxDecimalDigits && d.Unscaled > -Pow10(MaxDecimalDigits) && d.Unscaled < Pow10(MaxDecimalDigits)
}

// text returns the exact lexical form used for domain coercion.
func (d Decimal) text() string {
	if d.Raw != "" {
		return d.Raw
	}
	return d.String()
}

// String renders the value with exactly d.Scale fractional digits. This fixed
// form is the persisted and wire representation of a decimal.
func (d Decimal) String() string {
	digits := strconv.FormatInt(d.Unscaled, 10)
	if d.Scale == 0 {
		return digits
	}
	negative := strings.HasPrefix(digits, "-")
	if negative {
		digits = digits[1:]
	}
	for int32(len(digits)) <= d.Scale {
		digits = "0" + digits
	}
	cut := len(digits) - int(d.Scale)
	out := digits[:cut] + "." + digits[cut:]
	if negative {
		out = "-" + out
	}
	return out
}

// Canonical returns the shortest representation of the numeric value, with
// trailing fractional zeros removed. It is the identity form used by keys.
func (d Decimal) Canonical() string {
	if d.Raw != "" {
		// Unresolved literal: never treat it as a stored identity.
		return "invalid:" + d.Raw
	}
	unscaled, scale := d.Unscaled, d.Scale
	if unscaled == 0 {
		return "0"
	}
	for scale > 0 && unscaled%10 == 0 {
		unscaled /= 10
		scale--
	}
	return Decimal{Unscaled: unscaled, Scale: scale}.String()
}

// decimalParts splits an exact decimal literal into sign, significant digits
// (no leading zeros) and the natural scale (digits after the point). It rejects
// exponents, whitespace and empty components so callers cannot smuggle a
// binary-float spelling into the exact path.
func decimalParts(text string) (negative bool, digits string, scale int, err error) {
	if text == "" {
		return false, "", 0, fmt.Errorf("empty decimal")
	}
	if len(text) > MaxDecimalLiteral {
		return false, "", 0, fmt.Errorf("decimal literal exceeds %d bytes", MaxDecimalLiteral)
	}
	at := 0
	if text[at] == '+' || text[at] == '-' {
		negative = text[at] == '-'
		at++
	}
	intStart := at
	for at < len(text) && text[at] >= '0' && text[at] <= '9' {
		at++
	}
	if at == intStart {
		return false, "", 0, fmt.Errorf("invalid decimal literal")
	}
	intPart := text[intStart:at]
	fracPart := ""
	if at < len(text) && text[at] == '.' {
		at++
		fracStart := at
		for at < len(text) && text[at] >= '0' && text[at] <= '9' {
			at++
		}
		if at == fracStart {
			return false, "", 0, fmt.Errorf("decimal point requires a following digit")
		}
		fracPart = text[fracStart:at]
	}
	if at != len(text) {
		return false, "", 0, fmt.Errorf("invalid decimal literal")
	}
	digits = strings.TrimLeft(intPart+fracPart, "0")
	scale = len(fracPart)
	for scale > 0 && len(digits) > 0 && digits[len(digits)-1] == '0' {
		digits = digits[:len(digits)-1]
		scale--
	}
	return negative, digits, scale, nil
}

// ParseDecimal converts an exact literal into its natural (minimal-scale)
// form. It does not resize the value and applies no rounding.
func ParseDecimal(text string) (Decimal, error) {
	negative, digits, scale, err := decimalParts(text)
	if err != nil {
		return Decimal{}, err
	}
	if digits == "" {
		return Decimal{}, nil
	}
	if len(digits) > MaxDecimalDigits {
		return Decimal{}, fmt.Errorf("decimal exceeds %d significant digits", MaxDecimalDigits)
	}
	if scale > MaxDecimalDigits {
		return Decimal{}, fmt.Errorf("decimal scale exceeds %d", MaxDecimalDigits)
	}
	unscaled, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Decimal{}, fmt.Errorf("invalid decimal literal")
	}
	if negative {
		unscaled = -unscaled
	}
	return Decimal{Unscaled: unscaled, Scale: int32(scale)}, nil
}

// NormalizeDecimal turns an exact literal into a value of the declared column
// domain DECIMAL(precision, scale). Exactly representable inputs are rescaled;
// inputs needing more fractional digits follow mode; inputs outside the
// declared digit range fail rather than silently losing information.
func NormalizeDecimal(text string, precision, scale int, mode Rounding) (Decimal, error) {
	if precision < 1 || precision > MaxDecimalDigits {
		return Decimal{}, fmt.Errorf("decimal precision must be 1..%d", MaxDecimalDigits)
	}
	if scale < 0 || scale > precision {
		return Decimal{}, fmt.Errorf("decimal scale must be 0..precision")
	}
	if _, err := ParseRounding(string(mode)); err != nil {
		return Decimal{}, err
	}
	negative, digits, naturalScale, err := decimalParts(text)
	if err != nil {
		return Decimal{}, err
	}
	if digits == "" {
		return Decimal{Unscaled: 0, Scale: int32(scale)}, nil
	}
	if naturalScale <= scale {
		// Pad with exact zeros to the declared scale.
		shift := scale - naturalScale
		if len(digits)+shift > precision {
			return Decimal{}, fmt.Errorf("value exceeds decimal(%d,%d)", precision, scale)
		}
		unscaled, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return Decimal{}, fmt.Errorf("invalid decimal literal")
		}
		if shift > 0 {
			unscaled *= Pow10(shift)
		}
		if negative {
			unscaled = -unscaled
		}
		return Decimal{Unscaled: unscaled, Scale: int32(scale)}, nil
	}

	// Drop the excess fractional digits. The significant digits are aligned to
	// the natural scale first, so an implicit leading zero participates in the
	// retained part instead of the rounding decision (0.005 at scale 2 is
	// decided by the digit 5, not by a zero).
	drop := naturalScale - scale
	if len(digits) < naturalScale {
		digits = strings.Repeat("0", naturalScale-len(digits)) + digits
	}
	kept := digits[:len(digits)-drop]
	dropped := digits[len(digits)-drop:]
	if strings.TrimLeft(kept, "0") == "" {
		kept = ""
	}
	if len(kept) > precision {
		return Decimal{}, fmt.Errorf("value exceeds decimal(%d,%d)", precision, scale)
	}
	var unscaled int64
	if kept != "" {
		unscaled, err = strconv.ParseInt(kept, 10, 64)
		if err != nil {
			return Decimal{}, fmt.Errorf("invalid decimal literal")
		}
	}
	roundUp := false
	switch mode {
	case RoundTruncate:
	case RoundHalfUp:
		roundUp = dropped[0] >= '5'
	case RoundHalfEven:
		switch {
		case dropped[0] > '5':
			roundUp = true
		case dropped[0] < '5':
			roundUp = false
		default:
			restNonZero := false
			for i := 1; i < len(dropped); i++ {
				if dropped[i] != '0' {
					restNonZero = true
					break
				}
			}
			roundUp = restNonZero || unscaled%2 != 0
		}
	default: // RoundReject
		for i := 0; i < len(dropped); i++ {
			if dropped[i] != '0' {
				return Decimal{}, fmt.Errorf("value requires rounding at scale %d and column rounding is reject", scale)
			}
		}
	}
	if roundUp {
		unscaled++
	}
	if negative {
		unscaled = -unscaled
	}
	if unscaled <= -Pow10(precision) || unscaled >= Pow10(precision) {
		return Decimal{}, fmt.Errorf("value exceeds decimal(%d,%d)", precision, scale)
	}
	return Decimal{Unscaled: unscaled, Scale: int32(scale)}, nil
}

// CoerceDecimal turns a typed input value into the declared decimal domain.
// Integers and quoted decimal literals are accepted; decimal values already
// produced by SQL or a previous coercion are rescaled through the same path.
func CoerceDecimal(v Value, precision, scale int, mode Rounding) (Value, error) {
	var text string
	switch v.Kind {
	case "int":
		text = strconv.FormatInt(v.Int, 10)
	case "text":
		text = v.Text
	case "decimal":
		text = v.Dec.text()
	default:
		return Value{}, fmt.Errorf("decimal requires an integer or quoted decimal value")
	}
	d, err := NormalizeDecimal(text, precision, scale, mode)
	if err != nil {
		return Value{}, err
	}
	return Value{Kind: "decimal", Dec: d}, nil
}

func (v Value) MarshalJSON() ([]byte, error) {
	switch v.Kind {
	case "", "null":
		return []byte("null"), nil
	case "int":
		return []byte(strconv.FormatInt(v.Int, 10)), nil
	case "text":
		return json.Marshal(v.Text)
	case "bool":
		return json.Marshal(v.Bool)
	case "decimal":
		if !v.Dec.Valid() {
			return nil, fmt.Errorf("invalid decimal value")
		}
		return json.Marshal(v.Dec.String())
	default:
		return nil, fmt.Errorf("unknown value kind %q", v.Kind)
	}
}
func (v *Value) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var raw any
	if err := d.Decode(&raw); err != nil {
		return err
	}
	*v = Value{Kind: "null"}
	switch x := raw.(type) {
	case nil:
	case string:
		v.Kind = "text"
		v.Text = x
	case bool:
		v.Kind = "bool"
		v.Bool = x
	case json.Number:
		text := x.String()
		n, err := strconv.ParseInt(text, 10, 64)
		if err == nil {
			v.Kind = "int"
			v.Int = n
			break
		}
		// The lexical form is parsed directly as an exact decimal; the value
		// never passes through a binary float. An exponent or an over-wide
		// literal stays as raw text so column coercion makes the final
		// accept/round/reject decision with the declared domain.
		dec, decErr := ParseDecimal(text)
		if decErr != nil {
			v.Kind = "decimal"
			v.Dec = Decimal{Raw: text}
			break
		}
		v.Kind = "decimal"
		v.Dec = dec
	default:
		return fmt.Errorf("scalar value required")
	}
	return nil
}
func (v Value) Null() bool { return v.Kind == "" || v.Kind == "null" }
func (v Value) Compare(w Value) (int, error) {
	if v.Null() || w.Null() {
		return 0, fmt.Errorf("null has no predicate ordering")
	}
	if v.Kind != w.Kind {
		return 0, fmt.Errorf("incompatible types %s and %s", v.Kind, w.Kind)
	}
	switch v.Kind {
	case "int":
		if v.Int < w.Int {
			return -1, nil
		}
		if v.Int > w.Int {
			return 1, nil
		}
	case "text":
		if v.Text < w.Text {
			return -1, nil
		}
		if v.Text > w.Text {
			return 1, nil
		}
	case "bool":
		if !v.Bool && w.Bool {
			return -1, nil
		}
		if v.Bool && !w.Bool {
			return 1, nil
		}
	case "decimal":
		return compareDecimals(v.Dec, w.Dec)
	}
	return 0, nil
}

func compareDecimals(a, b Decimal) (int, error) {
	if !a.Valid() || !b.Valid() {
		return 0, fmt.Errorf("invalid decimal value")
	}
	if a.Scale == b.Scale {
		if a.Unscaled < b.Unscaled {
			return -1, nil
		}
		if a.Unscaled > b.Unscaled {
			return 1, nil
		}
		return 0, nil
	}
	common := a.Scale
	if b.Scale > common {
		common = b.Scale
	}
	x := new(big.Int).SetInt64(a.Unscaled)
	x.Mul(x, big.NewInt(Pow10(int(common-a.Scale))))
	y := new(big.Int).SetInt64(b.Unscaled)
	y.Mul(y, big.NewInt(Pow10(int(common-b.Scale))))
	return x.Cmp(y), nil
}

func (v Value) Key() string {
	if v.Kind == "decimal" {
		return "decimal:" + v.Dec.Canonical()
	}
	b, _ := v.MarshalJSON()
	return v.Kind + ":" + string(b)
}
func Integer(n int64) Value { return Value{Kind: "int", Int: n} }
