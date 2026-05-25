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

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <BrowserRouter>
      <App />
    </BrowserRouter>
  </React.StrictMode>,
);
