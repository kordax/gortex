import copy
import unittest

from report import compare_reports, parse_benchmarks


class ReportTests(unittest.TestCase):
    def test_repeated_measurements_and_scientific_notation(self):
        rows = parse_benchmarks("""goos: linux
BenchmarkSearch/files=32-4 10 100 ns/op 64 B/op 2 allocs/op
BenchmarkSearch/files=32-4 20 3e2 ns/op 64 B/op 2 allocs/op
BenchmarkSearch/files=32-4 15 200 ns/op 64 B/op 2 allocs/op
PASS
""")
        result = rows["BenchmarkSearch/files=32-4"]
        self.assertEqual(result["median"]["ns/op"], 200)
        self.assertEqual(result["min_ns_per_op"], 100)
        self.assertEqual(result["max_ns_per_op"], 300)
        self.assertEqual(len(result["samples"]), 3)

    def test_missing_or_invalid_results_fail(self):
        for text in ("PASS", "BenchmarkSearch-4 0 5 ns/op", "BenchmarkSearch-4 1 nan ns/op", "BenchmarkSearch-4 1 -1 ns/op", "BenchmarkSearch-4 1 5 ns/op dangling"):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_benchmarks(text)

    def test_inconsistent_units_fail(self):
        with self.assertRaises(ValueError):
            parse_benchmarks("BenchmarkSearch-4 1 100 ns/op\nBenchmarkSearch-4 2 90 ns/op 1 B/op")

    def sample(self):
        return {"metadata": {"go_version": "go1.27.2", "gomaxprocs": 4, "machine": "amd64"}, "fixture_version": 1,
                "micro": parse_benchmarks("BenchmarkSearch-4 10 100 ns/op 0 B/op")}

    def test_comparison_preserves_sign_and_zero_baseline(self):
        baseline = self.sample()
        candidate = copy.deepcopy(baseline)
        candidate["micro"]["BenchmarkSearch-4"]["median"]["ns/op"] = 80
        result = compare_reports(baseline, candidate)["BenchmarkSearch-4"]
        self.assertAlmostEqual(result["ns/op"]["change_percent"], -20)
        self.assertIsNone(result["B/op"]["change_percent"])

    def test_incompatible_environment_or_fixture_fails(self):
        for field in ("go_version", "gomaxprocs", "machine", "cpu_model", "logical_cpus", "package_parallelism"):
            candidate = self.sample()
            candidate["metadata"][field] = "different"
            with self.assertRaises(ValueError):
                compare_reports(self.sample(), candidate)
        candidate = self.sample()
        candidate["fixture_version"] = 2
        with self.assertRaises(ValueError):
            compare_reports(self.sample(), candidate)

    def test_no_common_cases_fail(self):
        candidate = self.sample()
        candidate["micro"] = {}
        with self.assertRaises(ValueError):
            compare_reports(self.sample(), candidate)


if __name__ == "__main__":
    unittest.main()
