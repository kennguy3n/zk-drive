// Test setup — initializes the i18next singleton before any test
// module loads a component, so useTranslation() resolves keys to
// English copy from src/i18n/locales/en.json rather than echoing
// the raw "namespace.key" identifier. Without this, every test
// that asserts on rendered button labels or status copy would
// have to match the key string, which silently desynchronises
// from the production app whenever a key gets renamed.
//
// We avoid importing src/i18n/index.ts directly because that
// module installs the browser-language-detector plugin, which
// reads navigator.language and localStorage — not what we want
// in jsdom. Instead, mirror the same i18next.init call without
// the detector so tests run deterministically with English.

// --- localStorage polyfill ---
// jsdom provides localStorage, but vi.useFakeTimers() in Vitest 4.x
// can clobber the global, causing `localStorage is not available`
// errors in tests that use fake timers (ThemeProvider, CommandPaletteDialog).
// This stable in-memory polyfill is installed before any test runs and
// survives fake-timer activation.
if (!globalThis.localStorage) {
  const store = new Map<string, string>();
  const localStoragePolyfill: Storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => store.set(key, String(value)),
    removeItem: (key: string) => store.delete(key),
    clear: () => store.clear(),
    key: (index: number) => Array.from(store.keys())[index] ?? null,
    get length() {
      return store.size;
    },
  };
  Object.defineProperty(globalThis, "localStorage", {
    value: localStoragePolyfill,
    writable: true,
    configurable: true,
  });
}

import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import en from "../i18n/locales/en.json";

if (!i18n.isInitialized) {
  // eslint-disable-next-line @typescript-eslint/no-floating-promises
  i18n.use(initReactI18next).init({
    resources: { en: { translation: en } },
    lng: "en",
    fallbackLng: "en",
    interpolation: { escapeValue: false },
    // returnNull=false makes missing keys fall back to the key
    // string itself rather than null — useful in tests where a
    // missing translation should fail loudly with a recognizable
    // identifier instead of crashing on .toString of null.
    returnNull: false,
    // returnEmptyString=false mirrors the production config at
    // src/i18n/index.ts:38 so the deploy-skew guard in
    // translateApiError (`translated !== key`) is exercised by
    // the same branch in tests as in production. Without this,
    // `t(unknownKey, {defaultValue: ""})` would return "" in
    // tests (the defaultValue path) but the raw key string in
    // production (returnEmptyString rejecting "") — the test config
    // mirrors production to avoid that divergence.
    returnEmptyString: false,
  });
}
