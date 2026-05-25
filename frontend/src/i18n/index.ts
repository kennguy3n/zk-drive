// i18n is the global react-i18next entry point. It is initialised
// once on app boot (see main.tsx) and exposes a default-export
// `i18n` instance so callers that need imperative access (e.g.
// the api/client error-code mapping helper at api/errors.ts) can
// reach the translator without going through the React hook.
//
// Detection strategy: i18next-browser-languagedetector reads in
// order [localStorage, navigator] so a user who picks a language
// in settings (write `i18nextLng` to localStorage) overrides the
// browser default. We do NOT detect from the URL path or cookie
// today — those tend to fight with React Router and the auth
// cookie, respectively, and the workspace admin will eventually
// drive the default via the workspace.search_language column
// (WS1) anyway.
//
// Adding a new locale: drop a JSON next to `en.json`, import it
// here, and add it to the `resources` map. Keys must be a
// superset of the English file or the missing-key fallback fires.
import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import LanguageDetector from "i18next-browser-languagedetector";

import en from "./locales/en.json";

void i18n
  .use(LanguageDetector)
  .use(initReactI18next)
  .init({
    resources: {
      en: { translation: en },
    },
    fallbackLng: "en",
    // We treat missing keys as a development error (caught by
    // the locale-coverage vitest, see i18n.test.tsx) rather than
    // silently rendering the key. Returning the key in production
    // would be visible garbage; instead we let the fallbackLng
    // path kick in and render the English copy.
    returnEmptyString: false,
    interpolation: {
      // React already escapes — i18next would double-escape
      // ampersands and quotes if we left this on.
      escapeValue: false,
    },
    detection: {
      order: ["localStorage", "navigator"],
      lookupLocalStorage: "i18nextLng",
      caches: ["localStorage"],
    },
  });

export default i18n;
