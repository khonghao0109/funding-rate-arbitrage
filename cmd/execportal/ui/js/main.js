// Entry point: wait for the portal, then start the four tabs and the header.

import { api, schedule } from "./core.js";
import { shell } from "./shell.js";
import { initExecution, onStatus, onPortalDown } from "./execution.js";
import { initScanner } from "./scanner.js";
import { initPaper } from "./paper.js";
import { initCrowding } from "./crowding.js";

const STATUS_MS = 5000;

async function boot() {
  shell.init();

  let first = await api("/api/status");
  while (!first.ok) {
    shell.renderPortalDown(first.body.error_vi || "không kết nối được portal");
    await new Promise((resolve) => setTimeout(resolve, 3000));
    first = await api("/api/status");
  }
  shell.renderStatus(first.body);

  initExecution(first.body);
  initScanner();
  initPaper();
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
