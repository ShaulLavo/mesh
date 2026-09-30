# A little color room

A temporary app with a Python HTTP server and SQLite storage. No dependencies
beyond Python 3. Shuffle asks the server for a palette. Save adds it to the shared
favorites collection. Another browser can load or remove those favorites.

```sh
mesh app create local ./examples/palette-server --run 'python3 server.py' --port 18747
```

For an existing app:

```sh
mesh app update local APP_ID ./examples/palette-server --run 'python3 server.py' --port 18747
```

The database lives in the managed source copy. Favorites survive page reloads
and worker or daemon restarts. Updating the source replaces this database;
expiry or deletion removes it with the server and source. All admitted visitors
share the same collection. This demo has no separate per-user accounts.
