import React from "react";
import ReactDOM from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import App from "./App";
import "./index.css";
// Side-effect import: initialises i18next + the React binding so
// useTranslation() works inside any component rendered below. Must
// be imported BEFORE the first <App/> render or the initial paint
// shows raw translation keys until i18next finishes its async init
// pass.
import "./i18n";
import { ThemeProvider } from "./theme/ThemeProvider";
import { ToastProvider } from "./components/ui/toast";
import { DialogsProvider } from "./components/ui/dialogs";
import { FeaturesProvider } from "./hooks/useFeatures";
import { CommandPaletteProvider } from "./components/CommandPalette";
import { OfflineIndicator } from "./components/OfflineIndicator";

// Provider order (outermost first):
//   ThemeProvider          — applies the .dark class + tokens app-wide.
//   ToastProvider          — imperative toast API for any descendant.
//   DialogsProvider        — imperative confirm()/prompt()/pickResource()
//                            replacing native dialogs; below ToastProvider
//                            so dialogs can raise toasts if needed.
//   FeaturesProvider       — fetches /api/features on login for gating.
//   CommandPaletteProvider — global Cmd+K palette (needs the router +
//                            auth + features above it).
// These are mounted here rather than inside App.tsx to keep App.tsx
// focused on routing and providers.
ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <ThemeProvider>
        <ToastProvider>
          <DialogsProvider>
            <FeaturesProvider>
              <CommandPaletteProvider>
                <OfflineIndicator />
                <App />
              </CommandPaletteProvider>
            </FeaturesProvider>
          </DialogsProvider>
        </ToastProvider>
      </ThemeProvider>
    </BrowserRouter>
  </React.StrictMode>,
);
