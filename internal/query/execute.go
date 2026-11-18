package query

import (
	"errors"
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"sort"
)

var errNoSum = errors.New("no non-null inputs")

func Execute(r catalog.Relation, q Request, rows map[string]datum.Row, plan Plan, keys []string) (Result, error) {
	result := Result{Rows: []datum.Row{}, Plan: plan}
	if plan.Access == "scan" {
		keys = datum.Keys(rows)
	}
	selected := make([]datum.Row, 0)
	for _, key := range keys {
		row, ok := rows[key]
		if !ok {
			continue
		}
		result.Scanned++
		if Matches(row, q.Where) {
			selected = append(selected, row)
		}
	}
	result.Matched = len(selected)
	if q.Aggregate != nil {
		row, err := reduce(r, selected, *q.Aggregate)
		if err != nil {
			return result, err
		}
		result.Rows = append(result.Rows, row)
		return result, nil
	}
	sort.Slice(selected, func(i, j int) bool {
		for _, o := range q.Order {
			n := datum.Order(selected[i][o.Column], selected[j][o.Column])
			if n != 0 {
				if o.Desc {
					return n > 0
				}
				return n < 0
			}
		}
		return datum.Order(selected[i][r.Primary], selected[j][r.Primary]) < 0
	})
	limit := q.Limit
	if limit == 0 {
		limit = 1000
	}
	if len(selected) > limit {
		selected = selected[:limit]
		result.Truncated = true
	}
	for _, row := range selected {
		if len(q.Columns) == 0 {
			result.Rows = append(result.Rows, datum.Clone(row))
		} else {
			projection := datum.Row{}
			for _, name := range q.Columns {
				projection[name] = row[name]
			}
			result.Rows = append(result.Rows, projection)
		}
	}
	return result, nil
}
func reduce(r catalog.Relation, rows []datum.Row, a Aggregate) (datum.Row, error) {
	if a.Function == "count" {
		var count int64
		for _, row := range rows {
			v := row[a.Column]
			if a.Column != "" && a.Column != "*" && v.Null() {
				continue
			}
			count++
		}
		return datum.Row{"count": datum.Integer(count)}, nil
	}
	column, ok := r.Column(a.Column)
	if !ok {
		return nil, fmt.Errorf("unknown aggregate column")
	}
	if !column.IsDecimal() {
		// The legacy integer interface is preserved verbatim: a sum that
		// leaves int64 is an overflow error, and the result is a JSON integer.
		var total int64
		count := 0
		for _, row := range rows {
			v := row[a.Column]
			if v.Null() {
				continue
			}
			count++
			if v.Kind != "int" {
				return nil, fmt.Errorf("sum requires integer values")
			}
			next := total + v.Int
			if v.Int > 0 && next < total || v.Int < 0 && next > total {
				return nil, fmt.Errorf("sum overflow")
			}
			total = next
		}
		if count == 0 {
			return datum.Row{"sum": datum.Value{Kind: "null"}}, nil
		}
		return datum.Row{"sum": datum.Integer(total)}, nil
	}
	total, err := sumExact(rows, a.Column)
	if err != nil {
		if err == errNoSum {
			return datum.Row{"sum": datum.Value{Kind: "null"}}, nil
		}
		return nil, err
	}
	// The sum of DECIMAL(p,s) values fits DECIMAL(18,s); render it at the
	// declared scale so SQL and structured aggregation agree and the result
	// can be compared directly with column values.
	rescaled, err := rescaleExact(total, column.Scale)
	if err != nil {
		return nil, err
	}
	return datum.Row{"sum": datum.Value{Kind: "decimal", Dec: rescaled}}, nil
}

// rescaleExact pads with zeros, or narrows only when the dropped digits are
// zero. A sum that would lose a non-zero digit is refused rather than rounded.
func rescaleExact(d datum.Decimal, scale int) (datum.Decimal, error) {
	if int(d.Scale) < scale {
		shift := scale - int(d.Scale)
		limit := datum.Pow10(datum.MaxDecimalDigits)
		for i := 0; i < shift; i++ {
			if d.Unscaled > limit/10 || d.Unscaled < -limit/10 {
				return datum.Decimal{}, fmt.Errorf("sum overflow")
			}
			d.Unscaled *= 10
		}
		d.Scale = int32(scale)
		return d, nil
	}
	for d.Scale > int32(scale) {
		if d.Unscaled%10 != 0 {
			return datum.Decimal{}, fmt.Errorf("sum cannot be represented at column scale %d", scale)
		}
		d.Unscaled /= 10
		d.Scale--
	}
	return d, nil
}

// sumExact accumulates exact base-10 values in integer form scaled by the
// widest input scale. Addition never rounds; a result that leaves the
// supported digit range is reported instead of wrapping.
func sumExact(rows []datum.Row, column string) (datum.Decimal, error) {
	scale := 0
	inputs := make([]datum.Decimal, 0, len(rows))
	for _, row := range rows {
		v := row[column]
		if v.Null() {
			continue
		}
		var d datum.Decimal
		switch v.Kind {
		case "int":
			d = datum.Decimal{Unscaled: v.Int}
		case "decimal":
			d = v.Dec
		default:
			return datum.Decimal{}, fmt.Errorf("sum requires numeric values")
		}
		if int(d.Scale) > scale {
			scale = int(d.Scale)
		}
		inputs = append(inputs, d)
	}
	if len(inputs) == 0 {
		return datum.Decimal{}, errNoSum
	}
	limit := datum.Pow10(datum.MaxDecimalDigits)
	var total int64
	for _, d := range inputs {
		value := d.Unscaled
		for s := int(d.Scale); s < scale; s++ {
			if value > limit/10 || value < -limit/10 {
				return datum.Decimal{}, fmt.Errorf("sum overflow")
			}
			value *= 10
		}
		next := total + value
		if value > 0 && next < total || value < 0 && next > total {
			return datum.Decimal{}, fmt.Errorf("sum overflow")
		}
		total = next
	}
	if total <= -limit || total >= limit {
		return datum.Decimal{}, fmt.Errorf("sum overflow")
	}
	return datum.Decimal{Unscaled: total, Scale: int32(scale)}, nil
}
