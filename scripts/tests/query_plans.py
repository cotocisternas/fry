"""Keep plan measurements bound to the SQL actually executed by sqlc clients."""
import unittest

from scripts.bench import query_plans


class QuerySources(unittest.TestCase):
    def test_current_queries_use_generated_parameter_order(self):
        candidate, wrap, batch, hashes = query_plans.queries()
        self.assertIn('w.available_at<=$1', candidate)
        self.assertIn('w.organization>$2', candidate)
        self.assertIn('w.available_at<=$1', wrap)
        self.assertIn('w.organization<=$2', wrap)
        self.assertIn('w.organization=$1', batch)
        self.assertIn('w.available_at<=$2', batch)
        self.assertIn('LIMIT $3', batch)
        self.assertEqual(len(hashes), 2)
        self.assertTrue(all(len(value) == 64 for value in hashes.values()))

    def test_missing_duplicate_or_unannotated_query_cannot_produce_a_report(self):
        valid = 'const Example = `-- name: Example :one\nSELECT 42\n`\n'
        self.assertEqual(query_plans.generated_query(valid, 'Example'), 'SELECT 42')
        for source in ('', valid * 2, 'const Example = `SELECT 42`'):
            with self.subTest(source=source), self.assertRaises(ValueError):
                query_plans.generated_query(source, 'Example')


if __name__ == '__main__':
    unittest.main()
