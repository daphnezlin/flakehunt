package runner

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

type TestOutcome struct {
	Name   string // classname::name
	NodeID string // pytest node id, "" if the xml has no file path
	Failed bool   // failure or error
	Hung   bool   // failed because it hit the per-test timeout
	Skip   bool
}

// junitProblem is a <failure> or <error> element. pytest-timeout's message
// looks like "Failed: Timeout (>2.0s) from pytest-timeout."
type junitProblem struct {
	Message string `xml:"message,attr"`
}

func (p *junitProblem) hung() bool {
	return p != nil && strings.Contains(p.Message, "Timeout") && strings.Contains(p.Message, "pytest-timeout")
}

type junitCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	File      string        `xml:"file,attr"`
	Failure   *junitProblem `xml:"failure"`
	Error     *junitProblem `xml:"error"`
	Skipped   *struct{}     `xml:"skipped"`
}

type junitSuite struct {
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

// ParseJUnit handles <testsuites> or <testsuite> roots, including nested suites.
func ParseJUnit(r io.Reader) ([]TestOutcome, error) {
	var root junitSuite
	if err := xml.NewDecoder(r).Decode(&root); err != nil {
		return nil, fmt.Errorf("parse junit xml: %w", err)
	}
	var out []TestOutcome
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			out = append(out, TestOutcome{
				Name:   c.ClassName + "::" + c.Name,
				NodeID: NodeID(c.File, c.ClassName, c.Name),
				Failed: c.Failure != nil || c.Error != nil,
				Hung:   c.Failure.hung() || c.Error.hung(),
				Skip:   c.Skipped != nil,
			})
		}
		for _, child := range s.Suites {
			walk(child)
		}
	}
	walk(root)
	return out, nil
}
