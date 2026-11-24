package main

import (
	"encoding/xml"
	"fmt"
	"io"
)

// TestOutcome is the result of one test in one trial.
type TestOutcome struct {
	Name   string // "classname::name", unique per test
	Failed bool   // failure or error
	Skip   bool
}

type junitCase struct {
	ClassName string    `xml:"classname,attr"`
	Name      string    `xml:"name,attr"`
	Failure   *struct{} `xml:"failure"`
	Error     *struct{} `xml:"error"`
	Skipped   *struct{} `xml:"skipped"`
}

type junitSuite struct {
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

// ParseJUnit reads a JUnit XML report. It accepts both a <testsuites> root
// (pytest's default) and a bare <testsuite> root, including nested suites.
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
