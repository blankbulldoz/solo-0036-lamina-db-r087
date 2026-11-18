package catalog

import (
	"fmt"
	"lamina/internal/datum"
	"regexp"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func ValidName(s string) bool { return identifier.MatchString(s) }

// Column describes one typed column. Decimal columns carry the declared
// domain: Precision is the total number of coefficient digits, Scale is the
// fixed number of fractional digits, and Rounding decides what happens when an
// input needs more fractional digits than Scale. Every value written to or
// compared against the column is brought into exactly this domain first, so a
// stored value, a predicate and an index candidate all denote the same fact.
type Column struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Nullable  bool   `json:"nullable"`
	Precision int    `json:"precision,omitempty"`
	Scale     int    `json:"scale,omitempty"`
	Rounding  string `json:"rounding,omitempty"`
}

func (c Column) IsDecimal() bool { return c.Type == "decimal" }

// Mode returns the declared rounding policy; the empty string means reject.
func (c Column) Mode() datum.Rounding {
	mode, err := datum.ParseRounding(c.Rounding)
	if err != nil {
		return datum.RoundReject
	}
	return mode
}

type Relation struct {
	Name    string   `json:"name"`
	Primary string   `json:"primary"`
	Columns []Column `json:"columns"`
	Created uint64   `json:"created"`
}

func (r Relation) Validate() error {
	if !ValidName(r.Name) || !ValidName(r.Primary) {
		return fmt.Errorf("invalid relation or primary name")
	}
	if len(r.Columns) == 0 || len(r.Columns) > 64 {
		return fmt.Errorf("relation needs 1..64 columns")
	}
	seen := map[string]bool{}
	found := false
	for _, c := range r.Columns {
		if !ValidName(c.Name) || seen[c.Name] {
			return fmt.Errorf("invalid or duplicate column %q", c.Name)
		}
		seen[c.Name] = true
		switch c.Type {
		case "int", "text", "bool":
			if c.Precision != 0 || c.Scale != 0 || c.Rounding != "" {
				return fmt.Errorf("column %s: precision, scale and rounding apply only to decimal", c.Name)
			}
		case "decimal":
			if c.Precision < 1 || c.Precision > datum.MaxDecimalDigits {
				return fmt.Errorf("column %s: decimal precision must be 1..%d", c.Name, datum.MaxDecimalDigits)
			}
			if c.Scale < 0 || c.Scale > c.Precision {
				return fmt.Errorf("column %s: decimal scale must be 0..precision", c.Name)
			}
			if _, err := datum.ParseRounding(c.Rounding); err != nil {
				return fmt.Errorf("column %s: %s", c.Name, err)
			}
		default:
			return fmt.Errorf("unsupported type %q", c.Type)
		}
		if c.Name == r.Primary {
			found = true
			if c.Nullable || c.Type == "bool" {
				return fmt.Errorf("primary must be non-null int, text or decimal")
			}
			// Identity must never depend on a rounding decision: two spellings
			// of the same key have to denote the same stored key exactly.
			if c.IsDecimal() && c.Mode() != datum.RoundReject {
				return fmt.Errorf("primary decimal column must use reject rounding")
			}
		}
	}
	if !found {
		return fmt.Errorf("primary column does not exist")
	}
	return nil
}
func (r Relation) Column(name string) (Column, bool) {
	for _, c := range r.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// Coerce brings a non-null scalar into the column domain. strict forces exact
// representability (used for identity operations and for values restored from
// durable media) instead of applying the declared rounding policy.
func (c Column) Coerce(v datum.Value, strict bool) (datum.Value, error) {
	if c.IsDecimal() {
		mode := c.Mode()
		if strict {
			mode = datum.RoundReject
		}
		return datum.CoerceDecimal(v, c.Precision, c.Scale, mode)
	}
	if v.Kind != c.Type {
		return datum.Value{}, fmt.Errorf("column %s requires %s", c.Name, c.Type)
	}
	if v.Kind == "text" && len(v.Text) > 65536 {
		return datum.Value{}, fmt.Errorf("text exceeds 64KiB")
	}
	return v, nil
}

func (r Relation) normalize(row datum.Row, strict bool) (datum.Row, error) {
	for k := range row {
		if _, ok := r.Column(k); !ok {
			return nil, fmt.Errorf("unknown column %q", k)
		}
	}
	out := make(datum.Row, len(r.Columns))
	for _, c := range r.Columns {
		v, exists := row[c.Name]
		if !exists || v.Null() {
			if !c.Nullable {
				return nil, fmt.Errorf("column %s cannot be null", c.Name)
			}
			out[c.Name] = datum.Value{Kind: "null"}
			continue
		}
		// The primary key is always resolved exactly, even under a relation
		// whose other columns declare a rounding policy; and NormalizeStored
		// is strict for every column.
		colStrict := strict || c.Name == r.Primary
		coerced, err := c.Coerce(v, colStrict)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", c.Name, err)
		}
		out[c.Name] = coerced
	}
	return out, nil
}

// Normalize applies the relation's declared domains to a caller-supplied row.
func (r Relation) Normalize(row datum.Row) (datum.Row, error) { return r.normalize(row, false) }

// NormalizeStored re-reads a row from a journal record or image. It never
// rounds: durable bytes must already be exact for the declared domain, so a
// corrupted or incompatible image fails instead of silently changing a fact.
func (r Relation) NormalizeStored(row datum.Row) (datum.Row, error) { return r.normalize(row, true) }

// CoercePrimary validates and normalizes a primary-key literal.
func (r Relation) CoercePrimary(v datum.Value) (datum.Value, error) {
	c, ok := r.Column(r.Primary)
	if !ok {
		return datum.Value{}, fmt.Errorf("primary column does not exist")
	}
	if v.Null() {
		return datum.Value{}, fmt.Errorf("invalid primary value")
	}
	return c.Coerce(v, true)
}

func (r Relation) Key(row datum.Row) string { return row[r.Primary].Key() }
