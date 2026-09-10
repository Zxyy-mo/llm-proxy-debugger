# Panel sizing, scrolling and graph bounds

## Layout contract

- `App.vue` owns a `100dvh` application shell. Flex/split panel ancestors must explicitly allow shrinking with `min-h-0` and `min-w-0`.
- Each content region has its own native `.panel-scroll` container (`style.css`), with bounded dimensions and `overflow: auto`. Scrollable regions have accessible names and keyboard focus.
- Long request/response bodies use wrapping and JSON formatting. Avoid table-based intrinsic-width wrappers around arbitrary raw content.
- The rule editor is inside a scrollable tab panel; its toolbar is sticky. Opening it expands the utility panel. Its submit controls must stay reachable in a 320px-wide window and short landscape viewports.
- `TabsTrigger.vue` renders its slot directly in the flex button. A non-flex inner span puts a block SVG above the label and clips the label.
- Inactive `TabsContent` must use `display: none`; flex or scroll classes must not override hidden state.
- The interception workbench stays mounted with `v-show` when changing main views, so unsaved drafts survive navigation. Preserve local drafts across snapshot updates; reset them only after an acknowledged save or an explicit latest-version reload. A revision conflict disables stale submissions.
- Countdown values use server time; zero disables actions until the terminal decision arrives. Validate/save/release/cancel controls and the body editor must remain reachable with several queued requests, including short landscape viewports. Check the editor's usable height, not only button hit targets. Narrow portrait views use a queue selector; short landscape views use a side list and omit the duplicate current-request selector. The workbench can scroll when its split panel is smaller than the editor's minimum useful height.
- Payload panels stack on phones and tall tablets, and sit side by side in short landscape windows. The current-request selector remains available where the sidebar cannot provide navigation.
- The audit view hosts the cURL panel and the replay workbench behind a mode switch with an explicit source selector. Replay drafts and credential fields live in component memory only (never storage) and reset only when the trace or source changes. The run button stays disabled until every replayable credential is entered; a running replay shows its state, a cancel control and a link to the new call. Copy, run, validate and credential controls must stay reachable at the seven layout viewports, and the body editor keeps a usable height.
- JSON shown in editors is re-indented without parsing numbers (`formatBody`), so integers beyond 2^53 and escape sequences survive an edited replay or release.

## Graph contract

`CallGraph.vue` observes the actual canvas container with `useResizeObserver`, not just browser width. Fit mode reframes after topology and panel-size changes, including inspector toggles and splitter drags. Metric-only refreshes do not rearrange the graph. Manual navigation and selected-node focus are explicit modes; the visible Show All control returns to automatic fitting.

## Regression verification

- At 1600×1000, 1366×768, 1024×768, 768×600, 390×844, 320×640 and 844×390, scroll to the rule form's Submit button and check its bounds/hit target. Use trial clicks so layout QA does not create actual rules.
- Test long uninterrupted strings, hundreds of lines and at least 30 request entries. Verify end markers and the last entry can be reached, and that the page has no horizontal overflow.
- Measure every graph node against the canvas rectangle after window resizing, inspector opening/closing, splitter dragging, and adding graph nodes. Test a manual zoom followed by a refresh to ensure it is preserved.
- When browser routes provide synthetic fixtures, restore the routes and reload before handing back the preview.
