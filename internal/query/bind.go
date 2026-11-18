package query

import (
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
)

// Bind validates a request against the relation and rewrites every literal
// into the declared column domain. Predicate values therefore denote exactly
// the same facts as stored and indexed values, including decimal scale.
func Bind(r catalog.Relation, q *Request) error {
	if q.Limit < 0 || q.Limit > 10000 {
		return fmt.Errorf("limit must be 0..10000")
	}
	if len(q.Where) > 32 || len(q.Order) > 8 {
		return fmt.Errorf("query expression limit exceeded")
	}
	if q.Aggregate != nil && len(q.Columns) > 0 {
		return fmt.Errorf("projection and aggregate cannot be combined")
	}
	for _, name := range q.Columns {
		if _, ok := r.Column(name); !ok {
			return fmt.Errorf("unknown projection %s", name)
		}
	}
	for _, o := range q.Order {
		if _, ok := r.Column(o.Column); !ok {
			return fmt.Errorf("unknown ordering %s", o.Column)
		}
	}
	for i := range q.Where {
		p := &q.Where[i]
		c, ok := r.Column(p.Column)
		if !ok {
			return fmt.Errorf("unknown predicate %s", p.Column)
		}
		switch p.Op {
		case "=", "!=", "<", ">", "<=", ">=", "is_null", "not_null":
		default:
			return fmt.Errorf("unsupported comparison %s", p.Op)
		}
		if p.Op == "is_null" || p.Op == "not_null" || p.Value.Null() {
			continue
		}
		coerced, err := c.Coerce(p.Value, false)
		if err != nil {
			return fmt.Errorf("predicate type mismatch for %s: %w", p.Column, err)
		}
		p.Value = coerced
	}
	if a := q.Aggregate; a != nil {
		if a.Function != "count" && a.Function != "sum" {
			return fmt.Errorf("unsupported aggregate")
		}
		if a.Column == "*" || a.Column == "" {
			if a.Function != "count" {
				return fmt.Errorf("sum needs numeric column")
			}
		} else {
			c, ok := r.Column(a.Column)
			if !ok {
				return fmt.Errorf("unknown aggregate column")
			}
			if a.Function == "sum" && c.Type != "int" && c.Type != "decimal" {
				return fmt.Errorf("sum requires int or decimal column")
			}
		}
	}
	return nil
}
func Matches(row datum.Row, predicates []Predicate) bool {
	for _, p := range predicates {
		v := row[p.Column]
		if p.Op == "is_null" {
			if !v.Null() {
				return false
			}
			continue
		}
		if p.Op == "not_null" {
			if v.Null() {
				return false
			}
			continue
		}
		if v.Null() || p.Value.Null() {
			return false
		}
		n, err := v.Compare(p.Value)
		if err != nil {
			return false
		}
		pass := false
		switch p.Op {
		case "=":
			pass = n == 0
		case "!=":
			pass = n != 0
		case "<":
			pass = n < 0
		case ">":
			pass = n > 0
		case "<=":
			pass = n <= 0
		case ">=":
			pass = n >= 0
		}
		if !pass {
			return false
		}
	}
	return true
}
