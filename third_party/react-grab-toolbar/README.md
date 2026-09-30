# React Grab toolbar extraction

Source: https://github.com/aidenybai/react-grab
Commit: ea4bbec9e80f4802e8ae19ad18431edb9ddbb670
License: MIT, retained in LICENSE.

These original utilities supply Mesh's drag threshold, velocity projection, edge
snapping, and collapsed/expanded positioning. The application copies are adapted
by scripts/sync-app-pill.mjs: import boundaries are local, runtime inspection is
removed, collapsed dragging is enabled, interrupted snapping preserves the current position, and stale release velocity is discarded. Run that script to regenerate them.
Mesh owns the Solid component and its management controls.
