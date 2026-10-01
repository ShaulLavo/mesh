# React Grab toolbar extraction

Source: https://github.com/aidenybai/react-grab
Commit: ea4bbec9e80f4802e8ae19ad18431edb9ddbb670
License: MIT, retained in LICENSE.

create-toolbar-drag.ts is the source of web/app-pill/src/drag.ts, which keeps its
drag threshold, velocity tracking and click suppression. Mesh adds stale-velocity
discard, collapsed dragging and grabbing a snap in flight at its on-screen position.
Edge snapping and collapsed/expanded positioning are Mesh's own
(web/app-pill/src/dock.ts): React Grab's flush collapsed tab and padded release
target were two placement rules, which made the docked position depend on the path.
The pill's panel was React Grab's ToolbarContent until the dot became the pill's
only toggle. Its chevron and collapse animation no longer had a place, so Mesh
draws the panel in plain CSS with the same padding, radius, color and shadow,
and no longer copies ToolbarContent, IconChevron or cn.
