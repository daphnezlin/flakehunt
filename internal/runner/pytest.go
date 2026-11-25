package runner

import (
	"strconv"
	"strings"
)

// PytestArgs builds the pytest command for a trial. Shared by every runner so
// docker and (later) k8s run tests exactly the same way.
func PytestArgs(t Trial, junitPath string) []string {
	args := []string{
		"pytest", "-q", "-p", "no:cacheprovider",
		"--randomly-seed=" + strconv.FormatInt(t.Seed, 10),
		// xunit1 adds the file path to each test, needed to build node ids
		"-o", "junit_family=xunit1",
		"--junitxml=" + junitPath,
	}
	if len(t.Tests) > 0 {
		// run exactly these tests in exactly this order. the seed still
		// reseeds random per test, so random data matches the original run
		args = append(args, "--randomly-dont-reorganize")
		args = append(args, t.Tests...)
	}
	return args
}

// NodeID turns junit attributes into a pytest node id, e.g.
// file "tests/test_db.py", classname "tests.test_db.TestUsers", name "test_add"
// -> "tests/test_db.py::TestUsers::test_add". Returns "" without a file.
func NodeID(file, classname, name string) string {
	if file == "" {
		return ""
	}
	module := strings.ReplaceAll(strings.TrimSuffix(file, ".py"), "/", ".")
	id := file
	if rest, ok := strings.CutPrefix(classname, module+"."); ok && rest != "" {
		id += "::" + strings.ReplaceAll(rest, ".", "::")
	}
	return id + "::" + name
}
