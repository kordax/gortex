"""Fast validation for the explicitly isolated daemon scrape override."""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import unittest
import urllib.error

from smoke import isolated_metrics_port, request


class IsolatedTargetTest(unittest.TestCase):
    def test_default_uses_private_fixture(self):
        self.assertIsNone(isolated_metrics_port(""))

    def test_literal_loopback_http(self):
        for port in (1, 9474, 65535):
            with self.subTest(port=port):
                self.assertEqual(isolated_metrics_port(f"http://127.0.0.1:{port}/metrics"), port)

    def test_does_not_follow_redirects(self):
        class Redirect(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(302)
                self.send_header("Location", "http://192.0.2.1/never-request-this")
                self.end_headers()

            def log_message(self, *args):
                pass

        server = ThreadingHTTPServer(("127.0.0.1", 0), Redirect)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with self.assertRaises(urllib.error.HTTPError) as caught:
                request(f"http://127.0.0.1:{server.server_port}/metrics")
            self.assertEqual(caught.exception.code, 302)
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)

    def test_rejects_redirectable_or_ambiguous_targets(self):
        for value in (
            "http://localhost:9474/metrics",
            "http://0.0.0.0:9474/metrics",
            "http://192.168.1.1:9474/metrics",
            "http://127.0.0.1.example.com:9474/metrics",
            "http://user:password@127.0.0.1:9474/metrics",
            "http://127.0.0.1:9474/metrics?target=other",
            "http://127.0.0.1:9474/metrics#fragment",
            "https://127.0.0.1:9474/metrics",
            "http://127.0.0.1:0/metrics",
            "http://127.0.0.1:65536/metrics",
            "http://127.0.0.1:-1/metrics",
            "http://127.0.0.1/metrics",
            "http://127.0.0.1:9474/other",
        ):
            with self.subTest(value=value):
                with self.assertRaises(ValueError):
                    isolated_metrics_port(value)


if __name__ == "__main__":
    unittest.main()
