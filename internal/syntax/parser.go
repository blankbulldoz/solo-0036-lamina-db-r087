package syntax

import (
	"fmt"
	"lamina/internal/catalog"
	"lamina/internal/datum"
	"lamina/internal/query"
	"strconv"
)

type parser struct {
	tokens []token
	at     int
}

func Parse(sql string) (query.Request, error) {
	tokens, err := lex(sql)
	if err != nil {
		return query.Request{}, err
	}
	p := parser{tokens: tokens}
	q, err := p.selectQuery()
	if err != nil {
		return q, err
	}
	p.accept(";")
	if p.at != len(tokens) {
		return q, p.error("end of statement")
	}
	return q, nil
}
func (p *parser) error(expected string) error {
	if p.at >= len(p.tokens) {
		return fmt.Errorf("SQL: expected %s at end", expected)
	}
	t := p.tokens[p.at]
	return fmt.Errorf("SQL: expected %s at byte %d", expected, t.position)
}
func (p *parser) accept(text string) bool {
	if p.at < len(p.tokens) && !p.tokens[p.at].literal && p.tokens[p.at].text == text {
		p.at++
		return true
	}
	return false
}
func (p *parser) require(text string) error {
	if !p.accept(text) {
		return p.error(text)
	}
	return nil
}
func (p *parser) name() (string, error) {
	if p.at >= len(p.tokens) || p.tokens[p.at].literal || !catalog.ValidName(p.tokens[p.at].text) {
		return "", p.error("identifier")
	}
	name := p.tokens[p.at].text
	p.at++
	return name, nil
}
func (p *parser) scalar() (datum.Value, error) {
	if p.at >= len(p.tokens) {
		return datum.Value{}, p.error("scalar literal")
	}
	t := p.tokens[p.at]
	p.at++
	if t.literal {
		return datum.Value{Kind: "text", Text: t.text}, nil
	}
	switch t.text {
	case "null":
		return datum.Value{Kind: "null"}, nil
	case "true":
		return datum.Value{Kind: "bool", Bool: true}, nil
	case "false":
		return datum.Value{Kind: "bool"}, nil
	}
	n, err := strconv.ParseInt(t.text, 10, 64)
	if err == nil {
		return datum.Integer(n), nil
	}
	// A literal that is not an int64 may still be an exact decimal. It is
	// parsed from its lexical form; no binary float is involved.
	dec, decErr := datum.ParseDecimal(t.text)
	if decErr != nil {
		return datum.Value{}, fmt.Errorf("SQL: invalid numeric literal at byte %d: %s", t.position, decErr)
	}
	return datum.Value{Kind: "decimal", Dec: dec}, nil
}
func (p *parser) selectQuery() (query.Request, error) {
	q := query.Request{}
	if err := p.require("select"); err != nil {
		return q, err
	}
	if p.at < len(p.tokens) && (p.tokens[p.at].text == "count" || p.tokens[p.at].text == "sum") && !p.tokens[p.at].literal {
		fn := p.tokens[p.at].text
		p.at++
		if err := p.require("("); err != nil {
			return q, err
		}
		column := "*"
		if !p.accept("*") {
			var err error
			column, err = p.name()
			if err != nil {
				return q, err
			}
		}
		if err := p.require(")"); err != nil {
			return q, err
		}
		q.Aggregate = &query.Aggregate{Function: fn, Column: column}
	} else if !p.accept("*") {
		for {
			name, err := p.name()
			if err != nil {
				return q, err
			}
			q.Columns = append(q.Columns, name)
			if !p.accept(",") {
				break
			}
		}
	}
	if err := p.require("from"); err != nil {
		return q, err
	}
	name, err := p.name()
	if err != nil {
		return q, err
	}
	q.Relation = name
	if p.accept("where") {
		for {
			column, err := p.name()
			if err != nil {
				return q, err
			}
			pred := query.Predicate{Column: column}
			if p.accept("is") {
				not := p.accept("not")
				if err = p.require("null"); err != nil {
					return q, err
				}
				pred.Op = "is_null"
				if not {
					pred.Op = "not_null"
				}
			} else {
				if p.at >= len(p.tokens) {
					return q, p.error("comparison")
				}
				pred.Op = p.tokens[p.at].text
				if p.tokens[p.at].literal {
					return q, p.error("comparison")
				}
				p.at++
				if pred.Op == "<>" {
					pred.Op = "!="
				}
				pred.Value, err = p.scalar()
				if err != nil {
					return q, err
				}
			}
			q.Where = append(q.Where, pred)
			if !p.accept("and") {
				break
			}
		}
	}
	if p.accept("order") {
		if err = p.require("by"); err != nil {
			return q, err
		}
		for {
			column, err := p.name()
			if err != nil {
				return q, err
			}
			desc := p.accept("desc")
			if !desc {
				p.accept("asc")
			}
			q.Order = append(q.Order, query.Ordering{Column: column, Desc: desc})
			if !p.accept(",") {
				break
			}
		}
	}
	if p.accept("limit") {
		if p.at >= len(p.tokens) || p.tokens[p.at].literal {
			return q, p.error("integer limit")
		}
		n, err := strconv.Atoi(p.tokens[p.at].text)
		if err != nil || n <= 0 {
			return q, p.error("positive integer limit")
		}
		p.at++
		q.Limit = n
	}
	return q, nil
}
