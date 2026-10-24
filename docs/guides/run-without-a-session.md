# Run a command that ends with its connection

Use `mesh ssh` when you want a shell or one command on a host and no saved session. It goes through the host's private Mesh SSH listener (port 2222) and uses your existing device approval. It has no detach key, no reconnection and no recovery. The process ends when the connection closes.

Open a shell:

```bash
mesh ssh pc
```

Run one command:

```bash
mesh ssh pc -- df -h
```

Mesh passes command arguments directly, with no shell on the remote side. For pipes and other shell syntax, start a shell yourself:

```bash
mesh ssh pc -- sh -c 'df -h | head -n 2'
```

To keep a command running after you disconnect, use a normal session instead: `mesh pc -- command`. See [Getting started](../getting-started.md).
