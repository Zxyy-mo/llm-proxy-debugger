# Frontend clipping regression verification

## Reproduced defects

- At 1366×768, the rule Submit button was at y=900–932, outside the viewport. All relevant ancestors used overflow:hidden, so it was unreachable.
- The Console tab rendered its block SVG and text inside a non-flex span, clipping the label.
- Raw payload panels had poor sizing in narrow/short layouts; table-based scroll wrappers made arbitrary long content harder to constrain.
- The graph fitted only at initialization, so resizing panels or adding nodes could put content outside the canvas.

## Changes

- Added explicit flex minimum-size constraints and a dynamic-viewport application shell.
- Native named scroll regions for session/request lists, payloads, console and rules; formatted JSON and wrapping for long strings.
- Rule editing expands the bottom panel; the entire rule view scrolls, with a sticky toolbar and reachable actions.
- Tabs display labels on one line and hide inactive contents correctly.
- Payload direction accounts for both width and height; a request selector remains available in narrow layouts.
- The graph observes its container, automatically fits topology/size changes in full-graph mode, and preserves manual zoom/pan on refresh.

## Browser checks

- Rule form controls were within the visible panel and their hit targets were reachable at 1600×1000, 1366×768, 1024×768, 768×600, 390×844, 320×640 and 844×390. Trial clicks avoided creating real rules.
- Seven existing graph nodes were all inside the canvas at desktop/tablet/mobile/landscape sizes, after inspector toggles, and after dragging the splitter.
- A browser-only fixture added 25 nodes: all 32 nodes fitted with zero clipped nodes; refreshing preserved manual zoom. An optional screenshot of this stress fixture timed out, so this case was verified through geometry and interaction assertions.
- Long-content fixtures used 16,000- and 18,000-character unbroken strings, 150 response lines, 300 thinking lines and 30 extra request records. End markers were visible after scrolling at 1366×768, 768×600, 390×844, 320×640 and 844×390. The last request entry was reachable. No horizontal overflow occurred in the payload regions.
- Browser interception routes were removed and the page reloaded after fixture checks; the backend's real records and rules were preserved.
- `npm run build` and `git diff --check` passed.

Screenshots: `rules-1366.png`, `rules-390.png`, `long-content-1366.png`, `long-content-390.png`, `graph-resized.png`.
