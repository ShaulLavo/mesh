# Copy files between machines

`mesh cp` copies a file or directory between this machine and one enrolled host. It reuses the device approval and pinned host identity that commands use, so there is nothing extra to set up.

## Copy a file

Use `HOST:/absolute/path` for the remote side:

```bash
mesh cp ./artifact.wasm pc:/srv/work/artifact.wasm      # upload
mesh cp pc:/srv/work/report.txt ./report.txt            # download
```

## Copy a directory

Add `-r`:

```bash
mesh cp -r ./source pc:/srv/work/checkouts/             # upload
mesh cp -r pc:/srv/work/results ./results               # download
```

If the destination is an existing directory, the copy goes inside it under the source's base name. Otherwise the destination is the exact new path.

## What a copy does

- Existing files are replaced atomically. Directory contents merge, and files that exist only at the destination stay.
- Dotfiles, empty directories and permission bits are preserved.
- Symbolic links and special files are refused.
- No saved session is created, and temporary data is removed on disconnect.

## Know the limits

A file that changes while it is copied has the ordinary `cp` consistency. Stop writers first when you need a consistent checkout or build artifact.

An interrupted directory merge can leave files that already finished copying.

The receiver stages the compressed archive and the extracted files on the destination filesystem. Allow twice the copy size in free space, plus the largest file when you merge into an existing directory. Existing destination files keep their space until their replacements are ready. Memory stays bounded because the sender streams.

A forcibly killed receiver can leave a `.mesh-copy-stage-PID-*` directory beside the destination. It holds disposable copy data. Remove it after you confirm that its receiver has stopped and no copy is using it.

To run a command instead of copying a file, see [Run a command that ends with its connection](run-without-a-session.md).
