from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import unittest
from urllib.parse import parse_qsl, urlsplit

from euler_cloud import APIError, Client


class CutOffHandler(BaseHTTPRequestHandler):
    """Serves responses whose connection drops before the body is complete."""

    protocol_version = "HTTP/1.1"
    paths = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        type(self).paths.append(self.path)
        if self.path.endswith('/export.csv'):
            # Declares 100 bytes, sends 10, then the connection closes.
            self.send_response(200)
            self.send_header('Content-Type', 'text/csv')
            self.send_header('Content-Length', '100')
            self.send_header('Connection', 'close')
            self.end_headers()
            self.wfile.write(b'day,views\n')
            self.close_connection = True
        elif self.path.endswith('/chunked'):
            # One chunk arrives; the terminating zero-length chunk never does.
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Transfer-Encoding', 'chunked')
            self.send_header('Connection', 'close')
            self.end_headers()
            self.wfile.write(b'5\r\n{"ite\r\n')
            self.close_connection = True
        else:
            body = b'{"items":[]}'
            self.send_response(200)
            self.send_header('Content-Type', 'application/json')
            self.send_header('Content-Length', str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    def do_HEAD(self):
        type(self).paths.append(self.path)
        self.send_response(200)
        self.send_header('Content-Type', 'text/csv')
        self.send_header('Content-Length', '100')
        self.end_headers()


class TransportTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(('127.0.0.1', 0), CutOffHandler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        cls.thread.join()

    def setUp(self):
        CutOffHandler.paths.clear()
        self.client = Client(f'http://127.0.0.1:{self.server.server_port}', 'ten_a', 'prj_a',
                             'euler_sa_test-only-secret', allow_insecure_loopback=True)

    def test_truncated_body_is_a_transport_error_not_a_short_download(self):
        with self.assertRaises(APIError) as caught:
            self.client.request('statistics', path='/sites/site-1/export.csv')
        self.assertEqual((caught.exception.status, caught.exception.code), (0, 'transport_error'))

    def test_cut_off_chunked_body_is_a_transport_error(self):
        with self.assertRaises(APIError) as caught:
            self.client.request('database', path='/chunked')
        self.assertEqual((caught.exception.status, caught.exception.code), (0, 'transport_error'))

    def test_head_with_content_length_still_succeeds(self):
        response = self.client.request('statistics', 'HEAD', '/sites/site-1/export.csv')
        self.assertEqual((response.status, response.data), (200, None))

    def test_boolean_and_unset_query_values_follow_server_conventions(self):
        self.client.request('storage', path='/buckets/b-1/objects',
                            query={'recursive': True, 'after': None, 'limit': 50, 'prefix': ['docs/', None]})
        sent = parse_qsl(urlsplit(CutOffHandler.paths[-1]).query, keep_blank_values=True)
        self.assertEqual(sent, [('recursive', 'true'), ('limit', '50'), ('prefix', 'docs/')])
        self.client.request('storage', path='/buckets/b-1/objects', query={'recursive': False})
        self.assertEqual(urlsplit(CutOffHandler.paths[-1]).query, 'recursive=false')


if __name__ == '__main__':
    unittest.main()
