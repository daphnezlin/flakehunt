package runner

import (
	"encoding/xml"
	"fmt"
	"io"
)

type TestOutcome struct {
	Name   string // classname::name
	NodeID string // pytest node id, "" if the xml has no file path
	Failed bool   // failure or error
	Skip   bool
}

type junitCase struct {
	ClassName string    `xml:"classname,attr"`
	Name      string    `xml:"name,attr"`
	File      string    `xml:"file,attr"`
	Failure   *struct{} `xml:"failure"`
	Error     *struct{} `xml:"error"`
	Skipped   *struct{} `xml:"skipped"`
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
