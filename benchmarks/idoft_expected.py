"""Write a flakehunt expected.json for one project from IDoFT's py-data.csv.

    python3 benchmarks/idoft_expected.py py-data.csv huashengdun/webssh > benchmarks/webssh/expected.json

IDoFT labels a flaky test OD (order-dependent, OD-Vic and OD-Brit included) or
NOD (flaky in the same order). flakehunt has more specific causes, so NOD
becomes "not order-dependent", which score matches against any of them.
NIO tests are left out: they fail when run twice in one process, which
flakehunt never does.
"""

import csv
import json
import sys


def flakehunt_name(node_id):
    # tests/test_app.py::TestAppBasic::test_x -> tests.test_app.TestAppBasic::test_x
    path, *rest = node_id.split("::")
    module = path.removesuffix(".py").replace("/", ".")
    return ".".join([module] + rest[:-1]) + "::" + rest[-1]


def main():
    csv_path, project = sys.argv[1], sys.argv[2]
    expected = {}
    with open(csv_path, newline="") as f:
        for row in csv.DictReader(f):
            if not row["Project URL"].rstrip("/").endswith("/" + project):
                continue
            category = row["Category"]
            if category.startswith("OD"):
                cause = "order-dependent"
            elif category == "NOD":
                cause = "not order-dependent"
            else:
                continue
            test = row["Pytest Test Name (PathToFile::TestClass::TestMethod or PathToFile::TestMethod)"]
            expected[flakehunt_name(test)] = {"cause": cause}
    if not expected:
        sys.exit(f"no OD or NOD tests for {project} in {csv_path}")
    json.dump(dict(sorted(expected.items())), sys.stdout, indent=2)
    print()


if __name__ == "__main__":
    main()
