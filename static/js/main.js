// Entry point: wait for the portal, then start every tab and the header.

import { $, api, schedule } from "./core.js";
import { shell } from "./shell.js";
import { initI18nAndTheme } from "./i18n.js";
import { initExecution, onStatus, onPortalDown } from "./execution.js";
import { initMaster } from "./master.js";
import { initScanner } from "./scanner.js";
import { initPaper } from "./paper.js";
import { initRadar } from "./radar.js";
import { initCrossperp } from "./crossperp.js";
import { initBacktest } from "./backtest.js";
import { initCrowding } from "./crowding.js";

const STATUS_MS = 5000;

async function boot() {
  initI18nAndTheme();
  shell.init();

  let first = await api("/api/status");
  // Every portal answer carries X-Execution-Mode, and /api/status is always
  // routed. A bare 404 means a plain file server — cmd/scanner serves static/
  // from disk — so say where the page runs instead of polling it forever.
  if (first.status === 404 && !(first.headers && first.headers.get("X-Execution-Mode"))) {
    $("served-elsewhere").hidden = false;
    shell.renderPortalDown("máy chủ này không phải cmd/execportal");
    return;
  }
  while (!first.ok) {
    shell.renderPortalDown(first.body.error_vi || "không kết nối được portal");
    await new Promise((resolve) => setTimeout(resolve, 3000));
    first = await api("/api/status");
  }
  shell.renderStatus(first.body);

  initMaster();
  initExecution(first.body);
  initScanner();
  initPaper();
  initRadar();
  initCrossperp();
  initBacktest();
  initCrowding();
  shell.start();

  schedule(async () => {
    const r = await api("/api/status");
    if (!r.ok) {
      const message = r.body.error_vi || "mất kết nối portal";
      shell.renderPortalDown(message);
      onPortalDown(message);
      return;
    }
    shell.renderStatus(r.body);
    onStatus(r.body);
  }, () => STATUS_MS);
}

boot();
