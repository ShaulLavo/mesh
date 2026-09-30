import json
import os
import random
import re
import sqlite3
from contextlib import closing
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit


ROOT = Path(__file__).resolve().parent
DATABASE = ROOT / 'favorites.sqlite3'
PALETTES = [
    ('Soft landing', ['#E8DFD0', '#C9C8AC', '#88958D', '#505D58', '#313D39']),
    ('After the rain', ['#E3EAF1', '#A6C0CD', '#667FA0', '#384C73', '#202D45']),
    ('Sunday market', ['#F2E6CE', '#D6AA78', '#BD7153', '#824F45', '#46372F']),
    ('Quiet studio', ['#EBE7F0', '#C9BFD5', '#9C8FAA', '#6F617E', '#42394D']),
    ('Tangerine afternoon', ['#FFF0D5', '#ECCA89', '#D89A57', '#BA5E38', '#5E372B']),
    ('Greenhouse', ['#E9EBD9', '#BCC5A1', '#859C74', '#536B57', '#293F35']),
    ('Rosewater', ['#F0E3DF', '#DBBFBB', '#BF8F91', '#925F6B', '#513845']),
    ('Coastal walk', ['#EFEBD9', '#CECB9F', '#93B4A8', '#507F83', '#2B4D58']),
]


def colors(value):
    if not isinstance(value, list) or len(value) != 5:
        raise ValueError('Choose five colors.')
    if not all(isinstance(color, str) and re.fullmatch(r'#[0-9a-fA-F]{6}', color) for color in value):
        raise ValueError('Colors must use six-digit hex codes.')
    return [color.upper() for color in value]


def favorite(row):
    return {'id': row[0], 'name': row[1], 'colors': json.loads(row[2])}


def generate(payload):
    previous = colors(payload.get('colors'))
    locked = payload.get('locked')
    if not isinstance(locked, list) or len(locked) != 5 or not all(type(item) is bool for item in locked):
        raise ValueError('Choose which colors to lock.')
    choices = [item for item in PALETTES if item[0] != payload.get('name')]
    name, selected = random.choice(choices)
    return {'name': name, 'colors': [previous[i] if locked[i] else selected[i] for i in range(5)]}


class Room(BaseHTTPRequestHandler):
    def send(self, status, body, content_type='application/json; charset=utf-8'):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Cache-Control', 'no-store')
        self.end_headers()
        if self.command != 'HEAD':
            self.wfile.write(data)

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == '/':
            self.send(200, (ROOT / 'index.html').read_bytes(), 'text/html; charset=utf-8')
            return
        if path == '/api/palette':
            name, selected = PALETTES[0]
            self.send(200, {'name': name, 'colors': selected})
            return
        if path == '/api/favorites':
            with closing(sqlite3.connect(DATABASE)) as database, database:
                rows = database.execute('SELECT id, name, colors FROM favorites ORDER BY id DESC').fetchall()
            self.send(200, [favorite(row) for row in rows])
            return
        self.send(404, {'error': 'Not found.'})

    def read_payload(self):
        length = int(self.headers.get('Content-Length', '0'))
        if not 0 < length <= 8192:
            raise ValueError('Request must contain at most 8 KB of JSON.')
        value = json.loads(self.rfile.read(length))
        if not isinstance(value, dict):
            raise ValueError('Expected a JSON object.')
        return value

    def do_POST(self):
        try:
            self.post(self.read_payload())
        except (ValueError, UnicodeDecodeError) as error:
            self.send(400, {'error': str(error)})

    def post(self, payload):
        path = urlsplit(self.path).path
        if path == '/api/palette':
            self.send(200, generate(payload))
            return
        if path != '/api/favorites':
            self.send(404, {'error': 'Not found.'})
            return
        selected = colors(payload.get('colors'))
        name = payload.get('name')
        if not isinstance(name, str) or not 1 <= len(name.strip()) <= 80:
            raise ValueError('Name must contain 1 to 80 characters.')
        with closing(sqlite3.connect(DATABASE)) as database, database:
            database.execute('BEGIN IMMEDIATE')
            if database.execute('SELECT COUNT(*) FROM favorites').fetchone()[0] >= 50:
                raise ValueError('This room has 50 favorites. Remove one before saving another.')
            cursor = database.execute('INSERT INTO favorites (name, colors) VALUES (?, ?)', (name.strip(), json.dumps(selected)))
        self.send(201, {'id': cursor.lastrowid, 'name': name.strip(), 'colors': selected})

    def do_DELETE(self):
        match = re.fullmatch(r'/api/favorites/([0-9]+)', urlsplit(self.path).path)
        if not match or len(match[1]) > 10:
            self.send(404, {'error': 'Not found.'})
            return
        with closing(sqlite3.connect(DATABASE)) as database, database:
            cursor = database.execute('DELETE FROM favorites WHERE id = ?', (int(match[1]),))
        self.send(200 if cursor.rowcount else 404, {'deleted': bool(cursor.rowcount)})


if __name__ == '__main__':
    with closing(sqlite3.connect(DATABASE)) as database, database:
        database.execute('CREATE TABLE IF NOT EXISTS favorites (id INTEGER PRIMARY KEY, name TEXT NOT NULL, colors TEXT NOT NULL)')
    ThreadingHTTPServer((os.environ.get('HOST', '127.0.0.1'), int(os.environ.get('PORT', '18747'))), Room).serve_forever()
