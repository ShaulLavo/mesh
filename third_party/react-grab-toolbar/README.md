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
The pill shell now uses the original ToolbarContent, IconChevron and cn sources
from the same commit. The sync script changes only their import paths. Mesh renders ToolbarContent
expanded only; its collapsed state is the Mesh dot. Tailwind
compiles their original spacing and edge orientation.
Mesh supplies the actions and disables position transitions during viewport changes.
