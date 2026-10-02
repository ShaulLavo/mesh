# Dashboard

## Themes

`mesh dashboard` shows the fleet with the OLED high-contrast theme by default.
Use `mesh dashboard --wall --theme rose-pine` for a fullscreen view with a
one-run override. Valid themes are `current`, `rose-pine`, `rose-pine-moon`,
`oled`, `kanagawa`, and `gruvbox-material`.

Save the selection in the existing Mesh config, `~/.config/mesh/hosts.json`
(or `$XDG_CONFIG_HOME/mesh/hosts.json`; `MESH_CONFIG_DIR` overrides the directory).
Keep the existing `version` and `hosts` entries and add:

```json
"dashboard": { "theme": "oled" }
```

`--theme` overrides the saved setting. Truecolor terminals use the theme's
background and colors; ANSI16 consoles use semantic slots and faint graph fills.
The terminal's original color defaults return when the dashboard exits.

Kanagawa and Gruvbox Material status colors fail common color-vision checks; status words still communicate state.
