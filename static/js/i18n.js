// i18n & Theme Engine: Hỗ trợ chuyển đổi 3 ngôn ngữ (Việt - Anh - Trung)
// và 2 chế độ hiển thị Dark/Light Mode, lưu trữ tùy chọn vào localStorage.

import { $, setText } from "./core.js";

const LANG_KEY = "portal.lang";
const THEME_KEY = "portal.theme";

export const DICTIONARY = {
  vi: {
    brand_title: "ARBITRAGE PRO",
    brand_subtitle: "Trạm Giao Dịch Trọng Tài",
    nav_master: "Tổng Hành Dinh",
    nav_scanner: "Market Scanner",
    nav_radar: "Radar Chéo Sàn",
    nav_crossperp: "Động Cơ 2 (Perp-Perp)",
    nav_autotrade: "Động Cơ 1 (Cash & Carry)",
    nav_manual: "Thao Tác Lệnh",
    nav_paper: "Sổ Cái & PnL",
    nav_backtest: "Backtest 3 Năm",
    nav_crowding: "Đám Đông",
    theme_dark: "Tối",
    theme_light: "Sáng",
    total_equity: "TỔNG VỐN LIÊN SÀN (USD)",
    binance_spot: "Binance Spot · USDT",
    binance_futures: "Binance Futures · USDT",
    bybit_linear: "Bybit Linear · USD",
    margin_guard: "VAN KÝ QUỸ KÉP",
    safe: "AN TOÀN",
    warn: "CẢNH BÁO",
    emergency: "NGẮT KHẨN CẤP",
    lock_matrix: "MA TRẬN KHÓA 13 CẶP COIN",
    active_positions: "VỊ THẾ LIÊN SÀN ĐANG MỞ",
    refresh: "Làm mới",
    syncing: "Đang đồng bộ…",
    idle: "Rảnh rỗi",
    occupied: "Đang bận",
    conflict: "Xung đột",
  },
  en: {
    brand_title: "ARBITRAGE PRO",
    brand_subtitle: "Institutional Terminal",
    nav_master: "Command Cockpit",
    nav_scanner: "Market Scanner",
    nav_radar: "Cross Radar",
    nav_crossperp: "Engine 2 (Perp-Perp)",
    nav_autotrade: "Engine 1 (Cash & Carry)",
    nav_manual: "Manual Desk",
    nav_paper: "Paper Ledger",
    nav_backtest: "3-Year Backtest",
    nav_crowding: "Crowding",
    theme_dark: "Dark",
    theme_light: "Light",
    total_equity: "COMBINED TOTAL EQUITY (USD)",
    binance_spot: "Binance Spot · USDT",
    binance_futures: "Binance Futures · USDT",
    bybit_linear: "Bybit Linear · USD",
    margin_guard: "DUAL MARGIN GUARD",
    safe: "SAFE",
    warn: "WARNING",
    emergency: "EMERGENCY HALT",
    lock_matrix: "13-PAIR EXCLUSIVE LOCK MATRIX",
    active_positions: "ACTIVE CROSS-VENUE POSITIONS",
    refresh: "Refresh",
    syncing: "Syncing…",
    idle: "Idle",
    occupied: "Held",
    conflict: "Conflict",
  },
  zh: {
    brand_title: "ARBITRAGE PRO",
    brand_subtitle: "机构级套利终端",
    nav_master: "总控中心",
    nav_scanner: "市场扫描仪",
    nav_radar: "跨交易所雷达",
    nav_crossperp: "引擎二 (双合约对冲)",
    nav_autotrade: "引擎一 (现货合约套利)",
    nav_manual: "手动执行",
    nav_paper: "模拟账本与收益",
    nav_backtest: "三年历史回测",
    nav_crowding: "情绪拥挤度",
    theme_dark: "深色",
    theme_light: "浅色",
    total_equity: "跨交易所总资产 (USD)",
    binance_spot: "币安现货 · USDT",
    binance_futures: "币安合约 · USDT",
    bybit_linear: "Bybit合约 · USD",
    margin_guard: "双重保证金保护器",
    safe: "安全",
    warn: "警告",
    emergency: "紧急熔断",
    lock_matrix: "13币对独占锁矩阵",
    active_positions: "当前活跃持仓",
    refresh: "刷新",
    syncing: "同步中…",
    idle: "空闲",
    occupied: "已占用",
    conflict: "冲突",
  },
};

let currentLang = "vi";
let currentTheme = "dark";

export function t(key) {
  const dict = DICTIONARY[currentLang] || DICTIONARY.vi;
  return dict[key] || DICTIONARY.vi[key] || key;
}

export function getLanguage() {
  return currentLang;
}

export function setLanguage(lang) {
  if (!DICTIONARY[lang]) return;
  currentLang = lang;
  try {
    localStorage.setItem(LANG_KEY, lang);
  } catch (_) {}

  document.documentElement.lang = lang;

  // Update all elements with data-i18n
  const elements = document.querySelectorAll("[data-i18n]");
  for (const node of elements) {
    const key = node.dataset.i18n;
    if (key) {
      const translated = t(key);
      if (node.textContent !== translated) {
        node.textContent = translated;
      }
    }
  }

  // Update language toggle buttons
  const langBtns = document.querySelectorAll(".lang-btn");
  for (const btn of langBtns) {
    const on = btn.dataset.lang === lang;
    btn.setAttribute("aria-selected", on ? "true" : "false");
    btn.classList.toggle("active", on);
  }
}

export function getTheme() {
  return currentTheme;
}

export function setTheme(theme) {
  currentTheme = theme === "light" ? "light" : "dark";
  try {
    localStorage.setItem(THEME_KEY, currentTheme);
  } catch (_) {}

  document.documentElement.dataset.theme = currentTheme;

  const btn = $("theme-toggle-btn");
  if (btn) {
    const isDark = currentTheme === "dark";
    setText("theme-toggle-icon", isDark ? "🌙" : "☀️");
    setText("theme-toggle-text", isDark ? t("theme_dark") : t("theme_light"));
  }
}

export function toggleTheme() {
  setTheme(currentTheme === "dark" ? "light" : "dark");
}

export function initI18nAndTheme() {
  let savedLang = "vi";
  let savedTheme = "dark";
  try {
    savedLang = localStorage.getItem(LANG_KEY) || "vi";
    savedTheme = localStorage.getItem(THEME_KEY) || "dark";
  } catch (_) {}

  setLanguage(DICTIONARY[savedLang] ? savedLang : "vi");
  setTheme(savedTheme);

  const themeBtn = $("theme-toggle-btn");
  if (themeBtn) {
    themeBtn.addEventListener("click", toggleTheme);
  }

  const langBtns = document.querySelectorAll(".lang-btn");
  for (const btn of langBtns) {
    btn.addEventListener("click", () => {
      if (btn.dataset.lang) {
        setLanguage(btn.dataset.lang);
      }
    });
  }
}
