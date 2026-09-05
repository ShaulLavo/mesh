#!/bin/sh
# Keep terminal-entry fixtures independent of personal shell startup files.
PS1='MESH_PROMPT> '
HISTFILE=/dev/null
export PS1 HISTFILE
exec /bin/bash --noprofile --norc -i
