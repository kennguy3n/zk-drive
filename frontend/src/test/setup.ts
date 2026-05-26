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
    // production (returnEmptyString rejecting ""). Devin Review
    // ANALYSIS_0001 on commit 500a3a8 flagged the divergence.
    returnEmptyString: false,
  });
}
