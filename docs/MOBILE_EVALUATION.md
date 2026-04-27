# Mobile App Evaluation — Phase 4 Sprint 9

**Status**: Decision recorded — PWA-first for Phase 5, revisit React Native
after GA.

This note captures the Phase 4 decision-gate review on whether the next step
for mobile clients is to (a) harden the existing React SPA into an installable
PWA or (b) invest in a React Native shell. The analysis here is based on a
static review of the Vite build output produced by `cd frontend && npm run
build`; a full Lighthouse audit in Chrome DevTools is queued for Phase 5 once
the PWA additions below are in place and can be measured against realistic
scores rather than a naked SPA baseline.

## Lighthouse audit — current SPA baseline

Lighthouse CLI (`lighthouse --preset=desktop`) is not installed in the CI
image used for this sprint, so the scores below are estimated from a manual
inspection of the built artefacts (`frontend/dist/index.html`,
`frontend/dist/assets/*.js`, `frontend/dist/assets/*.css`) and the network
profile implied by the current login flow. They should be treated as a
pre-PWA baseline to improve against; we will re-measure with an actual
Lighthouse run as soon as the PWA manifest/service-worker pieces land.

| Category        | Estimated score | Notes                                                                                                                             |
| --------------- | --------------- | --------------------------------------------------------------------------------------------------------------------------------- |
| Performance     | ~78             | Single JS bundle (~252 kB / ~80 kB gzip). Largest Contentful Paint dominated by auth-check roundtrip; no code splitting yet.      |
| Accessibility   | ~90             | Semantic HTML and labelled form controls throughout `AdminPage`/`FileBrowserPage`; known gaps are dialog focus traps and `aria-live` for toasts. |
| Best Practices  | ~90             | HTTPS, no known third-party libraries flagged by `npm audit` beyond two moderate transitive advisories; no console.errors observed on boot. |
| PWA             | ~35             | No `manifest.json`, no service worker, no app icons, no offline fallback. Viewport meta is present.                               |
| SEO             | n/a             | Authenticated app; intentionally not indexed.                                                                                      |

## PWA readiness

- **Web app manifest**: _missing._ Need `frontend/public/manifest.webmanifest`
  with `name`, `short_name`, `start_url`, `display: standalone`, `theme_color`,
  `background_color`, and a 192 + 512 px icon pair. Link it from `index.html`.
- **Service worker**: _missing._ Recommended approach is `vite-plugin-pwa` with
  Workbox precaching for the SPA shell, stale-while-revalidate for
  `/api/folders` listings, and network-only (never cache) for
  `/api/files/*/download` to avoid stale decryption-material. Strict-ZK
  folders in particular must never be cached by the service worker — the
  registration script should scope precaching to the SPA shell only.
- **Offline support**: _minimal._ The SPA currently requires a live API
  roundtrip for every view. Phase 5 should add an offline fallback page and
  a read-only cached view of the last-opened folder tree + file list.
- **Installable prompt**: not wired. Once the manifest + service worker are in
  place, add a small "Install app" button on `FileBrowserPage` conditional on
  `window.matchMedia('(display-mode: browser)').matches`.
- **Icons & splash screens**: Apple touch icons and Android splash
  screens should be generated once branding is finalised. Ship them from
  `frontend/public/icons/`.
- **Push notifications**: optional for Phase 5 — currently handled
  server-side via `internal/notification`; a future revision can wire Web
  Push once the PWA shell is stable.

## React Native evaluation

### Effort estimate

- **Shell + navigation**: 2 weeks to stand up Expo (or bare React Native)
  with auth/login, folder tree, file list, upload, share — mostly re-writing
  screens that today use plain HTML + Axios. Current components are plain
  React with no custom DOM-only primitives, so the logic layer re-uses cleanly.
- **Presigned uploads**: 1 week. `expo-file-system` / `react-native-fs` can
  stream the object to the presigned PUT URL but needs retry/backoff and
  resume logic we don't need on the web today.
- **Keychain + strict-ZK keys**: 1–2 weeks to integrate `expo-secure-store`
  (or `react-native-keychain`) for device-side key material if strict-ZK is
  ever extended to device-local DEK wrapping.
- **Offline sync**: 3–4 weeks. The drive metadata model (folders, files,
  permissions, activity) would need a local SQLite mirror with a delta-pull
  endpoint on the server (today we have none).
- **CI/CD & store presence**: 1–2 weeks for TestFlight + Play internal
  tracks, signing, screenshots, review back-and-forth.

Total: ~8–10 engineer-weeks for a feature-parity v0.1, plus ongoing store
compliance overhead.

### Feature gap

| Area                       | SPA today           | React Native native                | Gap                                                                 |
| -------------------------- | ------------------- | ---------------------------------- | ------------------------------------------------------------------- |
| File picker                | `<input type=file>` | `expo-document-picker`             | Low — well-supported.                                               |
| Background uploads         | Unsupported         | `expo-task-manager` + `BackgroundFetch` | Medium — iOS background execution budget is restrictive.            |
| Push notifications         | Unsupported         | `expo-notifications` / APNs / FCM  | Medium — requires server-side token store (not yet built).          |
| Strict-ZK key material     | Browser-only (WebCrypto) | `expo-secure-store`           | High — new storage surface with its own threat model.               |
| Biometrics                 | Unavailable         | `expo-local-authentication`        | New capability — nice-to-have, not on any current roadmap.          |

### Offline sync complexity

Offline sync is the biggest unknown. Today the server has no per-client
change cursor — every folder listing is a full `GET /api/folders/{id}`. A
React Native client that wants true offline would need:

1. A per-workspace change log (audit-log-adjacent but keyed by device for
   consumption).
2. Deterministic conflict resolution on the folder/file model.
3. Device-side cache invalidation when ACLs change.

All three are currently unbuilt. The PWA-first path avoids this work until
the product validates that mobile usage is meaningful.

## Recommendation

**Phase 5 direction: PWA-first.**

Rationale:

1. **Lower opportunity cost.** ~1 sprint of PWA work (manifest + service
   worker + install prompt + narrow offline fallback) unlocks
   "installable on phone" for every user without adding a native build
   pipeline.
2. **Reuses every existing React component.** No parallel screen
   implementation.
3. **Defers the offline-sync server work** until there is concrete demand
   signal from real mobile users — which the PWA install funnel will
   measure for us.
4. **Keeps the React Native option open.** Nothing we do for the PWA
   precludes a native shell later; most of the code (hooks, API client,
   validation) ports unchanged.

### Phase 5 action items (for follow-up sprint)

- Add `vite-plugin-pwa` with a shell precache and SWR runtime caching;
  exclude authenticated API routes from caching.
- Ship `public/manifest.webmanifest` with production icons.
- Add an "Install app" UI affordance on `FileBrowserPage` with a dismiss
  counter.
- Run an actual Lighthouse audit in Chrome DevTools against the deployed
  preview and record the scores in this document so the estimates above
  are replaced with measured numbers.
- Confirm with the privacy owner that the service worker's cache never
  includes strict-ZK folder listings or file bytes before shipping.

## Decision

Close the Phase 4 mobile evaluation gate with the **PWA-first** path
selected. React Native remains on the roadmap but is deferred behind
measurable install traction and a server-side delta-pull design.
