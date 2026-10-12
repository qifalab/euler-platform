import contextlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

from euler_cloud import APIError, Client
from euler_cloud.cli import _token


class Handler(BaseHTTPRequestHandler):
    requests = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        type(self).requests.append((self.path, dict(self.headers)))
        if self.path.endswith('/redirect'):
            self.send_response(302)
            self.send_header('Location', '/credential-trap')
            self.end_headers()
        elif self.path.endswith('/denied'):
            self.send_response(403)
            self.send_header('Content-Type', 'application/json')
            self.send_header('X-Request-ID', 'request-test-1')
            self.end_headers()
            self.wfile.write(b'{"error":{"code":"forbidden","message":"Operation denied"}}')
        else:
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('X-Request-ID', 'request-test-2')
            self.end_headers()
            self.wfile.write(json.dumps({'items': [{'id': 'db-example'}]}).encode())

    def do_POST(self):
        type(self).requests.append((self.path, dict(self.headers)))
        self.rfile.read(int(self.headers.get('Content-Length', '0')))
        self.send_response(503)
        self.end_headers()


class ClientTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def setUp(self):
        Handler.requests.clear()
        self.client = Client(f'http://127.0.0.1:{self.server.server_port}', 'ten_a', 'prj_a',
                             'euler_sa_test-only-secret', allow_insecure_loopback=True)

    def test_project_scoping_and_no_browser_credentials(self):
        self.assertEqual(self.client.databases(), {'items': [{'id': 'db-example'}]})
        path, headers = Handler.requests[0]
        self.assertEqual(path, '/api/v1/machine/tenants/ten_a/projects/prj_a/apps/database/databases')
        self.assertEqual(headers['Authorization'], 'Bearer euler_sa_test-only-secret')
        self.assertNotIn('Cookie', headers)
        self.assertNotIn('X-Csrf-Token', headers)
        self.assertNotIn('test-only-secret', repr(self.client))

    def test_redirect_does_not_forward_secret(self):
        with self.assertRaises(APIError) as caught:
            self.client.request('database', path='/redirect')
        self.assertEqual(caught.exception.status, 302)
        self.assertEqual(len(Handler.requests), 1)

    def test_error_correlation_and_no_mutation_retry(self):
        with self.assertRaises(APIError) as caught:
            self.client.request('database', path='/denied')
        self.assertEqual(caught.exception.request_id, 'request-test-1')
        self.assertEqual(caught.exception.code, 'forbidden')
        Handler.requests.clear()
        with self.assertRaises(APIError):
            self.client.request('database', 'POST', '/databases', data={'name': 'example'})
        self.assertEqual(len(Handler.requests), 1)

    def test_rejects_origin_or_path_escape(self):
        for path in ('//evil.test', 'https://evil.test', '/../session', '/%2e%2e/session',
                     '/%252e%252e/session', '/x?token=leak', '/x#y', '/x\\y'):
            with self.subTest(path=path), self.assertRaises(ValueError):
                self.client.request('database', path=path)
        for origin in ('http://example.test', 'https://user:pass@example.test',
                       'https://example.test/path', 'https://example.test?secret=x'):
            with self.subTest(origin=origin), self.assertRaises(ValueError):
                Client(origin, 'ten_a', 'prj_a', 'euler_sa_test', allow_insecure_loopback=True)
        self.assertEqual(Handler.requests, [])

    def test_query_is_encoded(self):
        self.client.request('statistics', path='/sites', query={'filter': 'a&b=汉字'})
        self.assertIn('filter=a%26b%3D', Handler.requests[0][0])

    def test_private_token_file(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / 'token'
            path.write_text('euler_sa_private\n')
            path.chmod(0o600)
            with patch.dict(os.environ, {'EULER_SERVICE_TOKEN_FILE': str(path)}):
                self.assertEqual(_token(), 'euler_sa_private')
                path.chmod(0o644)
                with self.assertRaises(ValueError):
                    _token()
            link = Path(folder) / 'link'
            link.symlink_to(path)
            with patch.dict(os.environ, {'EULER_SERVICE_TOKEN_FILE': str(link)}), self.assertRaises(ValueError):
                _token()


if __name__ == '__main__':
    unittest.main()
