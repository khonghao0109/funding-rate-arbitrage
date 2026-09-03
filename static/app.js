// Version of the backend WebSocket contract this file implements.
// Specification: docs/WS-CONTRACT.md
const WIRE_VERSION = 1;

// Storage key for the user's source toggles. Versioned because the pre-contract
// build stored a different shape - see loadStoredSourceToggles.
const SOURCE_TOGGLE_KEY = 'enabledSources.v2';

// Escapes a server-supplied string for interpolation into markup or an
// attribute. Labels and notes are Go literals today and move into config.yaml at
// step 1.4, at which point they stop being trusted input.
// Accepts a colour only in the forms the contract actually uses. A style
// attribute is a CSS context, where HTML escaping lets ';', ':' and '(' through.
function safeColor(value) {
    return /^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20})$/.test(String(value ?? '')) ? String(value) : '#888';
}

function esc(value) {
    return String(value ?? '').replace(/[&<>"']/g, c => ({
        '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;',
    })[c]);
}

class FuturesArbitrageScanner {
    constructor() {
        // Everything about a source - label, colour, market type, thresholds -
        // arrives in the `meta` message. Nothing about sources is hardcoded here.
        this.meta = null;
        // Offset between this browser's clock and the server's, refreshed from
        // every message envelope. The contract measures all ages against
        // server_time_ms precisely because the two clocks differ.
        this.clockOffsetMs = 0;
        this.sourceMeta = new Map();
        this.sourceStatus = new Map();
        this.symbols = [];
        this.costBasis = null;

        this.currentSymbol = null;
        this.sources = new Map();
        this.priceHistory = new Map();
        this.arbitrageOpportunities = [];
        this.currentSpreads = new Map();
        this.sourceListDirty = false;
        // Sources present in meta but absent from the latest price snapshot,
        // i.e. currently without a usable price. Treated like a disabled source
        // when drawing, so no stale line or last-value label is left behind.
        this.absentSources = new Set();
        this.maxHistoryPoints = 500; // Reduced from 1000
        this.maxOpportunities = 25; // Reduced from 50
        this.currentSort = { field: 'detected_at_ms', direction: 'desc' };
        this.minSpreadFilterPct = 0.05;
        
        // Only the user's explicit overrides are stored; the default for a source
        // that was never toggled comes from meta.sources[].enabled_by_default.
        this.enabledSources = this.loadStoredSourceToggles();
        
        this.chart = null;
        this.chartSeries = new Map(); // Map to store series for each source
        this.ws = null;
        this.reconnectAttempts = 0;
        this.maxReconnectAttempts = 10;
        
        // Performance optimization: throttling
        this.chartUpdatePending = false;
        this.sourceUpdatePending = false;
        this.opportunitiesUpdatePending = false;
        this.spreadsUpdatePending = false;
        
        // Realtime tracking
        this.isAtRealtime = true;
        this.lastUserScrollTime = 0;
        
        // WebSocket message batching
        this.messageQueue = [];
        this.processingMessages = false;
        
        this.init();
    }

    // Load the user's source toggles. Sources absent from storage fall back to
    // meta.sources[].enabled_by_default, so adding a venue server-side needs no
    // change here.
    loadStoredSourceToggles() {
        try {
            // The pre-contract build wrote a fully merged, all-true object for
            // every known source. Reading it back would pin every source on
            // forever and make enabled_by_default dead on arrival for anyone
            // who has used the dashboard before. Drop it once.
            if (localStorage.getItem(SOURCE_TOGGLE_KEY) === null) {
                localStorage.removeItem('enabledSources');
                return {};
            }
            const stored = localStorage.getItem(SOURCE_TOGGLE_KEY);
            return stored ? JSON.parse(stored) : {};
        } catch (error) {
            console.warn('Failed to load enabled sources from localStorage:', error);
            return {};
        }
    }
    
    // Save enabled sources to localStorage
    saveEnabledSources() {
        try {
            localStorage.setItem(SOURCE_TOGGLE_KEY, JSON.stringify(this.enabledSources));
        } catch (error) {
            console.warn('Failed to save enabled sources to localStorage:', error);
        }
    }
    
    // Toggle source visibility (now mainly used by non-UI code)
    toggleSource(source) {
        const oldState = this.isSourceEnabled(source);
        this.enabledSources[source] = !oldState;
        const newState = this.enabledSources[source];
        console.log(`Toggle ${source}: ${oldState} -> ${newState}`);
        
        this.saveEnabledSources();
        
        // Update all components immediately
        this.updateSourceList();
        this.performChartUpdate(); // Force immediate chart update, bypassing throttling
        this.updateSpreadsMatrix();
        this.updateOpportunitiesTable();
    }
    
    // Update source item opacity without full HTML recreation
    updateSourceVisibility() {
        const sourceItems = document.querySelectorAll('.source-item');
        sourceItems.forEach(item => {
            const checkbox = item.querySelector('input[type="checkbox"]');
            if (checkbox && checkbox.dataset.source) {
                const source = checkbox.dataset.source;
                const isEnabled = this.isSourceEnabled(source);
                item.style.opacity = isEnabled ? '1' : '0.4';
            }
        });
    }
    
    // Check if source is enabled
    isSourceEnabled(source) {
        if (Object.prototype.hasOwnProperty.call(this.enabledSources, source)) {
            return this.enabledSources[source] !== false;
        }
        const meta = this.sourceMeta.get(source);
        return meta ? meta.enabled_by_default !== false : true;
    }

    // ---- contract: meta -----------------------------------------------------
    // Builds every list the dashboard used to hardcode. See docs/WS-CONTRACT.md §3.
    applyMeta(meta) {
        if (meta.v !== WIRE_VERSION) {
            console.warn(`Contract version mismatch: server sent v${meta.v}, dashboard expects v${WIRE_VERSION}`);
        }

        this.meta = meta;
        this.symbols = meta.symbols || [];
        this.costBasis = meta.cost_basis || null;

        this.sourceMeta = new Map();
        (meta.sources || []).forEach(s => this.sourceMeta.set(s.source, s));

        if (typeof meta.alert_min_spread_pct === 'number') {
            const filterInput = document.getElementById('minSpreadFilter');
            if (filterInput && !filterInput.dataset.userEdited) {
                this.minSpreadFilterPct = meta.alert_min_spread_pct;
                filterInput.value = String(meta.alert_min_spread_pct);
            }
        }

        // Labels, colours and short names all come from meta, and the source
        // rows were built from the previous one. Rebuild them rather than
        // leaving stale labels on screen after a reconnect or a config reload.
        this.sourceListDirty = true;

        this.populateSymbolSelector();
        this.createChartSeries();
        this.renderCostBasisNote();
        this.updateSourceList();

        if (this.currentSymbol && !this.symbols.includes(this.currentSymbol)) {
            // The server no longer serves the symbol on screen; fall back rather
            // than leaving a selector that matches nothing.
            this.changeSymbol(meta.default_symbol || this.symbols[0] || null);
            const select = document.getElementById('symbolSelect');
            if (select && this.currentSymbol) select.value = this.currentSymbol;
        }

        if (!this.currentSymbol) {
            this.currentSymbol = meta.default_symbol || this.symbols[0] || null;
            const select = document.getElementById('symbolSelect');
            if (select && this.currentSymbol) select.value = this.currentSymbol;
            const status = document.getElementById('symbolStatus');
            if (status && this.currentSymbol) status.textContent = this.currentSymbol;
            this.updateChartTitle();
        }
    }

    populateSymbolSelector() {
        const select = document.getElementById('symbolSelect');
        if (!select) return;

        const previous = select.value;
        select.innerHTML = this.symbols
            .map(symbol => `<option value="${esc(symbol)}">${esc(symbol)}</option>`)
            .join('');
        if (this.symbols.includes(previous)) select.value = previous;
    }

    // Chart series cannot be created before meta arrives: their colour and line
    // style are part of the source metadata.
    createChartSeries() {
        if (!this.chart) return;

        // Drop series for sources the server no longer serves; leaving them
        // would keep a frozen line and last-value label that reads as live.
        for (const [source, series] of [...this.chartSeries.entries()]) {
            if (!this.sourceMeta.has(source)) {
                this.chart.removeSeries(series);
                this.chartSeries.delete(source);
                this.priceHistory.delete(source);
            }
        }

        const lineStyles = {
            solid: LightweightCharts.LineStyle.Solid,
            dashed: LightweightCharts.LineStyle.Dashed,
            dotted: LightweightCharts.LineStyle.Dotted,
        };

        this.sourceMeta.forEach((meta, source) => {
            const existing = this.chartSeries.get(source);
            if (existing) {
                // Presentation can change with a new meta; keep the drawn line
                // in step with the legend and the matrix.
                existing.applyOptions({
                    color: meta.color,
                    lineStyle: lineStyles[meta.line_style] ?? LightweightCharts.LineStyle.Solid,
                    crosshairMarkerBorderColor: meta.color,
                    crosshairMarkerBackgroundColor: meta.color,
                    title: meta.label,
                });
                return;
            }
            const series = this.chart.addLineSeries({
                color: meta.color,
                lineWidth: 2,
                lineStyle: lineStyles[meta.line_style] ?? LightweightCharts.LineStyle.Solid,
                crosshairMarkerVisible: true,
                crosshairMarkerRadius: 4,
                crosshairMarkerBorderColor: meta.color,
                crosshairMarkerBackgroundColor: meta.color,
                lastValueVisible: true,
                priceLineVisible: false,
                title: meta.label,
            });
            this.chartSeries.set(source, series);
        });
    }

    // States, in Vietnamese, what the displayed numbers have and have not had
    // deducted. Required by CLAUDE.md rule 2: a gross figure is never presented
    // as profit.
    renderCostBasisNote() {
        const el = document.getElementById('costBasisNote');
        if (!el || !this.costBasis) return;

        const names = {
            taker_fee: 'phí taker',
            maker_fee: 'phí maker',
            slippage: 'trượt giá',
            funding: 'phí funding',
        };
        const label = list => (list || []).map(k => names[k] || k).join(', ');

        const applied = this.costBasis.applied || [];
        // The server's own wording wins when it sends one; otherwise state what
        // its cost_basis implies. Never both - they say the same thing.
        const headline = this.costBasis.note_vi
            || (applied.length
                ? `Số hiển thị đã trừ: ${label(applied)}.`
                : 'Số hiển thị là chênh lệch THÔ, chưa trừ bất kỳ chi phí nào.');
        const excluded = (this.costBasis.excluded || []).length
            ? ` Chưa trừ: ${label(this.costBasis.excluded)}.`
            : '';
        el.innerHTML = `<strong>${esc(headline)}</strong>${esc(excluded)}`;
    }

    // Smart price formatting based on price value
    formatPrice(price) {
        if (price >= 1000) {
            return price.toFixed(2);
        } else if (price >= 100) {
            return price.toFixed(3);
        } else if (price >= 10) {
            return price.toFixed(4);
        } else if (price >= 1) {
            return price.toFixed(5);
        } else {
            return price.toFixed(6);
        }
    }

    init() {
        this.setupEventListeners();
        this.setupSourceCheckboxDelegation();
        this.setupChart();
        this.connectWebSocket();
        this.setupOpportunitiesTable();
    }

    setupEventListeners() {
        const symbolSelect = document.getElementById('symbolSelect');
        symbolSelect.addEventListener('change', (e) => {
            this.changeSymbol(e.target.value.toUpperCase());
        });

        window.addEventListener('resize', () => {
            if (this.chart) {
                this.chart.applyOptions({
                    width: this.getChartWidth(),
                    height: this.getChartHeight()
                });
            }
        });

        // Opportunities table event listeners
        const minSpreadFilter = document.getElementById('minSpreadFilter');
        minSpreadFilter.addEventListener('input', (e) => {
            e.target.dataset.userEdited = 'true';
            this.minSpreadFilterPct = parseFloat(e.target.value) || 0;
            this.updateOpportunitiesTable();
            this.updateSpreadsMatrix(); // Update matrix highlighting
        });

        const clearButton = document.getElementById('clearOpportunities');
        clearButton.addEventListener('click', () => {
            this.arbitrageOpportunities = [];
            this.updateOpportunitiesTable();
        });

        // Go to realtime button
        const goToRealtimeBtn = document.getElementById('goToRealtimeBtn');
        goToRealtimeBtn.addEventListener('click', () => {
            if (this.chart) {
                this.chart.timeScale().scrollToRealTime();
                this.isAtRealtime = true;
                this.lastUserScrollTime = 0;
            }
        });
    }
    
    setupSourceCheckboxDelegation() {
        const sourceList = document.getElementById('sourceList');
        
        sourceList.addEventListener('click', (event) => {
            if (event.target.type === 'checkbox' && event.target.dataset.source) {
                const source = event.target.dataset.source;
                console.log(`Checkbox click event for source: ${source}`);
                
                // Toggle state immediately without HTML recreation
                this.enabledSources[source] = !this.isSourceEnabled(source);
                console.log(`Toggle ${source}: -> ${this.enabledSources[source]}`);
                
                // Update checkbox state immediately
                event.target.checked = this.enabledSources[source];
                
                // Save and update other components
                this.saveEnabledSources();
                this.performChartUpdate();
                this.updateSpreadsMatrix();
                this.updateOpportunitiesTable();
                
                // Update source list opacity without full recreation
                this.updateSourceVisibility();
            }
        });
    }

    setupChart() {
        const chartContainer = document.getElementById('chart');
        
        // Create TradingView chart
        this.chart = LightweightCharts.createChart(chartContainer, {
            width: this.getChartWidth(),
            height: this.getChartHeight(),
            layout: {
                background: { color: '#0f0f0f' },
                textColor: '#e0e0e0',
                fontSize: 11,
                fontFamily: 'JetBrains Mono, Monaco, Consolas, monospace',
                attributionLogo: false
            },
            grid: {
                vertLines: { color: '#222' },
                horzLines: { color: '#222' },
            },
            crosshair: {
                mode: LightweightCharts.CrosshairMode.Normal,
            },
            rightPriceScale: {
                borderColor: '#444',
                textColor: '#888',
            },
            timeScale: {
                borderColor: '#444',
                textColor: '#888',
                timeVisible: true,
                secondsVisible: false,
            },
            handleScroll: {
                mouseWheel: true,
                pressedMouseMove: true,
            },
            handleScale: {
                axisPressedMouseMove: true,
                mouseWheel: true,
                pinch: true,
            },
        });

        // Series are created in createChartSeries() once meta has arrived: their
        // colour and line style come from the server, not from this file.


        // Track user interactions to determine if we should auto-scroll
        this.chart.timeScale().subscribeVisibleTimeRangeChange(() => {
            this.lastUserScrollTime = Date.now();
            this.isAtRealtime = false;
            
            // Reset realtime flag after some time of inactivity
            setTimeout(() => {
                if (Date.now() - this.lastUserScrollTime > 5000) {
                    this.isAtRealtime = true;
                }
            }, 5000);
        });

        this.updateChartTitle();
    }


    getChartWidth() {
        const chartContainer = document.getElementById('chart');
        return chartContainer ? Math.max(chartContainer.clientWidth - 40, 400) : 800;
    }

    getChartHeight() {
        const chartContainer = document.getElementById('chart');
        if (!chartContainer) return 400;
        
        const containerHeight = chartContainer.clientHeight;
        return Math.max(containerHeight - 40, 300);
    }


    connectWebSocket() {
        const wsStatus = document.getElementById('wsStatus');
        const wsStatusText = document.getElementById('wsStatusText');
        
        wsStatus.className = 'status-dot disconnected';
        wsStatusText.textContent = 'Connecting...';

        try {
            this.ws = new WebSocket(`ws://${window.location.host}/ws`);
            
            this.ws.onopen = () => {
                console.log('WebSocket connected');
                wsStatus.className = 'status-dot connected';
                wsStatusText.textContent = 'Connected';
                this.reconnectAttempts = 0;
            };

            this.ws.onmessage = (event) => {
                try {
                    const data = JSON.parse(event.data);
                    this.queueMessage(data);
                } catch (error) {
                    console.error('Error parsing WebSocket message:', error);
                }
            };

            this.ws.onclose = () => {
                console.log('WebSocket disconnected');
                wsStatus.className = 'status-dot disconnected';
                wsStatusText.textContent = 'Disconnected';
                this.invalidateFreshness();
                this.scheduleReconnect();
            };

            this.ws.onerror = (error) => {
                console.error('WebSocket error:', error);
                wsStatus.className = 'status-dot disconnected';
                wsStatusText.textContent = 'Error';
                this.invalidateFreshness();
            };

        } catch (error) {
            console.error('Failed to create WebSocket connection:', error);
            this.scheduleReconnect();
        }
    }

    scheduleReconnect() {
        if (this.reconnectAttempts < this.maxReconnectAttempts) {
            this.reconnectAttempts++;
            const delay = Math.min(1000 * Math.pow(2, this.reconnectAttempts), 30000);
            console.log(`Reconnecting in ${delay}ms (attempt ${this.reconnectAttempts})`);
            setTimeout(() => this.connectWebSocket(), delay);
        }
    }

    // Server time as this browser can best estimate it. Used for every age and
    // freshness calculation instead of Date.now().
    serverNowMs() {
        return Date.now() + this.clockOffsetMs;
    }

    // Called when our own connection to the scanner drops. Every "live" badge on
    // screen was justified by a message that is no longer arriving, so none of
    // them can be trusted any more. Prices stay visible for context; the claim
    // that they are fresh does not.
    invalidateFreshness() {
        this.sources.forEach(data => {
            data.status = 'unknown';
            data.ageMs = -1;
        });
        this.sourceStatus.forEach(status => {
            status.state = 'unknown';
        });
        // Absence was a claim about the venue. With our own socket down we no
        // longer know anything about any venue.
        this.absentSources.clear();
        // The chart must show the same gap a venue-side stall would produce;
        // otherwise a scanner outage is drawn as a continuous line.
        this.sourceMeta.forEach((_meta, source) => this.breakChartLine(source));
        this.sourceListDirty = true;
        this.updateSourceList();
    }

    queueMessage(data) {
        if (typeof data.server_time_ms === 'number') {
            this.clockOffsetMs = data.server_time_ms - Date.now();
        }
        this.messageQueue.push(data);
        if (!this.processingMessages) {
            this.processingMessages = true;
            setTimeout(() => this.processMessageQueue(), 50);
        }
    }
    
    processMessageQueue() {
        try {
            this.drainMessageQueue();
        } finally {
            // Never leave the pump latched: a throw here would stop every
            // further message while the UI still says "Connected".
            this.processingMessages = false;
        }

        // Schedule UI updates
        this.updateSourceList();
        this.updateChart();
    }

    drainMessageQueue() {
        const messagesToProcess = this.messageQueue.splice(0);
        
        // Group messages by type to batch similar operations
        const metaMessages = [];
        const arbitrageOpportunities = [];
        const spreadsUpdates = [];
        const otherMessages = [];
        
        messagesToProcess.forEach(data => {
            if (data.type === 'meta') {
                metaMessages.push(data);
            } else if (data.type === 'arbitrage') {
                arbitrageOpportunities.push(data);
            } else if (data.type === 'spreads') {
                spreadsUpdates.push(data);
            } else {
                otherMessages.push(data);
            }
        });
        
        // Meta first: every other message is rendered using the source metadata
        // it carries, so it must not be processed after them.
        metaMessages.forEach(data => this.applyMeta(data));
        
        // Process arbitrage opportunities
        arbitrageOpportunities.forEach(data => {
            this.handleArbitrageOpportunity(data.opportunity);
        });
        
        // Only the newest spreads message matters, but "newest" has to mean
        // newest *for the symbol on screen*: with several symbols ticking, the
        // last message in a batch is usually for one the user is not looking at,
        // and taking it blindly discards the update that would have been drawn.
        // "Newest" must be decided by the server's clock, not by position in the
        // batch: two ingestion goroutines broadcast concurrently, so a message
        // can arrive after one that was sent later.
        const relevant = spreadsUpdates.filter(m => m.symbol === this.currentSymbol);
        if (relevant.length > 0) {
            const newest = relevant.reduce((a, b) =>
                (b.server_time_ms ?? 0) >= (a.server_time_ms ?? 0) ? b : a);
            this.handleSpreadsUpdate(newest);
        }
        
        // Process other messages normally
        otherMessages.forEach(data => {
            this.handleWebSocketMessage(data);
        });
    }
    
    handleWebSocketMessage(data) {
        if (data.type === 'meta') {
            this.applyMeta(data);
        } else if (data.type === 'prices') {
            this.updatePrices(data);
        } else if (data.type === 'arbitrage') {
            this.handleArbitrageOpportunity(data.opportunity);
        } else if (data.type === 'spreads') {
            this.handleSpreadsUpdate(data);
        }
    }

    updatePrices(message) {
        if (message.source_status) {
            this.sourceStatus = new Map(Object.entries(message.source_status));
        }
        const serverTimeMs = message.server_time_ms;
        for (const [symbol, sourcePrices] of Object.entries(message.prices || {})) {
            if (symbol === this.currentSymbol) {
                for (const [source, point] of Object.entries(sourcePrices)) {
                    this.updateSourcePrice(source, point, serverTimeMs);
                    // Only a live price is a new observation. A stale one is the
                    // same frozen number resent: charting it draws a flat line
                    // that reads as a quiet market, and skipping it outright
                    // makes the chart interpolate straight across the outage.
                    // Break the line instead, so the gap is visible as a gap.
                    if (point.status === 'stale') {
                        this.breakChartLine(source);
                    } else {
                        this.addPriceToHistory(source, point.price);
                    }
                }
                // The message is a full snapshot, so absence means the source
                // has no usable price right now. Keeping its last row would
                // show a frozen price that looks healthy.
                this.absentSources = new Set();
                this.sourceMeta.forEach((_meta, source) => {
                    if (!(source in sourcePrices)) this.absentSources.add(source);
                });
                // A source with no price is not removed from the panel: a venue
                // that never connected, or died, has to stay visible and say so.
                // Only its chart line is dropped, above.

                // UI updates will be handled by processMessageQueue
                break;
            }
        }
    }

    addPriceToHistory(source, price, timestamp = null) {
        // The series is stamped with the server clock throughout, including the
        // whitespace points breakChartLine inserts. Mixing in Date.now() makes
        // times non-monotonic under clock skew, which either throws in
        // series.update() or silently hides the gap.
        const ts = timestamp ? timestamp / 1000 : this.serverNowMs() / 1000;
        
        if (!this.priceHistory.has(source)) {
            this.priceHistory.set(source, []);
        }

        const history = this.priceHistory.get(source);
        // Series times must strictly increase. serverNowMs() can step backwards
        // when the clock offset is re-estimated, and a non-monotonic point
        // throws in series.update() and corrupts the array for every later
        // setData().
        if (history.length > 0 && ts <= history[history.length - 1][0]) {
            return;
        }
        const newDataPoint = [ts, price];
        history.push(newDataPoint);

        if (history.length > this.maxHistoryPoints) {
            history.shift();
        }

        // If source is enabled, immediately update the chart series with the new data point
        if (this.isSourceEnabled(source) && !this.absentSources.has(source)) {
            const series = this.chartSeries.get(source);
            if (series) {
                const chartDataPoint = {
                    time: ts,
                    value: price
                };
                series.update(chartDataPoint);
                
                // Auto-scroll to realtime for new price updates if user is at realtime position
                if (this.chart && this.isAtRealtime && Date.now() - this.lastUserScrollTime > 3000) {
                    this.chart.timeScale().scrollToRealTime();
                }
            }
        }
    }

    // Records a gap in a source's series. TradingView treats a point with a time
    // but no value as whitespace, which ends the line rather than bridging it.
    breakChartLine(source) {
        const history = this.priceHistory.get(source);
        if (!history || history.length === 0) return;

        const last = history[history.length - 1];
        if (last[1] === null) return; // already broken

        const ts = this.serverNowMs() / 1000;
        if (ts <= last[0]) return; // series time must strictly increase
        history.push([ts, null]);

        const series = this.chartSeries.get(source);
        if (series && this.isSourceEnabled(source)) {
            series.update({ time: ts });
        }
    }

    updateSourcePrice(source, point, serverTimeMs) {
        const price = point.price;
        const previousPrice = this.sources.get(source)?.price || price;
        const change = price - previousPrice;
        const changePercent = previousPrice !== 0 ? (change / previousPrice) * 100 : 0;

        this.sources.set(source, {
            price: price,
            previousPrice: previousPrice,
            change: change,
            changePercent: changePercent,
            // Freshness is decided by the server: the browser clock is not
            // comparable with the server clock. See docs/WS-CONTRACT.md §4.1.
            status: point.status,
            ageMs: point.age_ms,
            venueTimeMs: point.venue_time_ms,
            recvAtMs: point.recv_at_ms,
            serverTimeMs: serverTimeMs,
            lastUpdate: Date.now()
        });

    }

    // Maps a source onto the CSS modifier for its status dot. Connection state
    // outranks data freshness: a disconnected venue is not merely stale.
    statusClass(source) {
        const state = this.sourceStatus.get(source)?.state;
        if (state === 'disconnected') return 'disconnected';
        if (state === 'reconnecting') return 'reconnecting';

        // Absent from the latest snapshot means the backend rejected whatever
        // this venue last sent. The row keeps its old price for context, but it
        // must not be drawn as live. A source that has never delivered at all is
        // waiting, not stale - a red dot there is a false alarm on every load.
        if (this.absentSources.has(source) && this.sources.has(source)) return 'stale';
        if (!this.sources.has(source)) return 'unknown';

        const status = this.sources.get(source)?.status;
        if (status === 'live') return 'live';
        if (status === 'stale') return 'stale';
        return 'unknown';
    }

    statusTitle(source) {
        const data = this.sources.get(source);
        if (!data) {
            const conn = this.sourceStatus.get(source);
            return conn && conn.state === 'disconnected'
                ? 'Sàn chưa gửi dữ liệu nào — coi như mất kết nối'
                : 'Đang chờ dữ liệu đầu tiên';
        }

        const parts = [];
        if (data.status === 'stale') {
            parts.push('Dữ liệu CŨ — đã loại khỏi so sánh');
        } else if (data.status === 'live') {
            parts.push('Dữ liệu mới');
        } else {
            parts.push('Chưa đo được độ mới của dữ liệu');
        }
        if (data.ageMs >= 0) parts.push(`nhận cách đây ${this.formatAge(data.ageMs)}`);

        const conn = this.sourceStatus.get(source);
        if (conn && conn.state === 'disconnected') {
            parts.push('Sàn không gửi gì cho bất kỳ cặp nào');
        }
        return parts.join(' · ');
    }

    formatAge(ageMs) {
        if (ageMs < 1000) return `${ageMs}ms`;
        if (ageMs < 60000) return `${(ageMs / 1000).toFixed(1)}s`;
        return `${Math.floor(ageMs / 60000)}m`;
    }

    // Short badge shown beside the price. A red dot alone is easy to miss, and a
    // frozen price that looks healthy is the failure this step exists to remove.
    statusBadge(source) {
        // Our own socket is down: nothing on screen can be called fresh, and the
        // venue is not the one at fault.
        if (this.ws && this.ws.readyState !== 1 && this.sources.has(source)) {
            return '<span class="source-badge waiting">MẤT KẾT NỐI MÁY CHỦ</span>';
        }
        const conn = this.sourceStatus.get(source);
        if (conn && conn.state === 'disconnected') {
            return '<span class="source-badge disconnected">MẤT KẾT NỐI</span>';
        }
        if (this.sources.get(source)?.status === 'stale') {
            return '<span class="source-badge stale">CŨ</span>';
        }
        if (!this.sources.has(source)) {
            return '<span class="source-badge waiting">CHỜ</span>';
        }
        // Still connected, but the last thing it sent was not a usable price.
        if (this.absentSources.has(source)) {
            return '<span class="source-badge stale">KHÔNG GIÁ</span>';
        }
        return '';
    }

    updateSourceList() {
        if (!this.sourceUpdatePending) {
            this.sourceUpdatePending = true;
            setTimeout(() => {
                try {
                    this.performSourceListUpdate();
                } finally {
                    // Never latch the panel: a throw here would stop every
                    // further redraw while the UI still says "Connected".
                    this.sourceUpdatePending = false;
                }
            }, 100);
        }
    }
    
    // Every source the server told us about gets a row, whether or not it has a
    // price. A venue that never connected is exactly the thing worth showing.
    displayedSources() {
        const seen = new Set();
        const out = [];
        this.sourceMeta.forEach((_meta, source) => {
            seen.add(source);
            out.push(source);
        });
        for (const source of this.sources.keys()) {
            if (!seen.has(source)) out.push(source);
        }
        return out;
    }

    performSourceListUpdate() {
        const sourceList = document.getElementById('sourceList');
        
        if (this.displayedSources().length === 0) {
            sourceList.innerHTML = '<div class="loading">No data available</div>';
            return;
        }

        // Check if we need to recreate HTML (structure changed) or just update prices
        const existingItems = [...sourceList.querySelectorAll('.source-item')];
        const rendered = new Set(existingItems.map(el => el.dataset.source));
        const wanted = this.displayedSources();
        const needsRecreation = this.sourceListDirty
            || rendered.size !== wanted.length
            || wanted.some(source => !rendered.has(source));
        this.sourceListDirty = false;
        
        if (needsRecreation) {
            console.log('Recreating source list HTML');
            this.recreateSourceList();
        } else {
            // Just update prices and visual states without recreating HTML
            this.updateSourcePrices();
        }
    }
    
    recreateSourceList() {
        const sourceList = document.getElementById('sourceList');

        // Registry order, so a source that drops out and comes back returns to
        // its own row instead of the bottom of the list.
        const ordered = this.displayedSources().sort((a, b) => {
            const ia = this.sourceOrderIndex(a);
            const ib = this.sourceOrderIndex(b);
            return ia === ib ? a.localeCompare(b) : ia - ib;
        });

        let html = '';
        for (const source of ordered) {
            const data = this.sources.get(source);
            const hasPrice = data !== undefined;
            const changeClass = hasPrice && data.change >= 0 ? 'up' : 'down';
            const changeSymbol = hasPrice && data.change >= 0 ? '↑' : '↓';
            const color = this.sourceMeta.get(source)?.color || '#888';
            const isEnabled = this.isSourceEnabled(source);
            const opacity = isEnabled ? '1' : '0.4';
            
            html += `
                <div class="source-item" data-source="${esc(source)}" style="opacity: ${opacity};">
                    <div style="display: flex; align-items: center; gap: 8px;">
                        <input type="checkbox" id="checkbox-${esc(source)}" ${isEnabled ? 'checked' : ''}
                               data-source="${esc(source)}"
                               style="margin-right: 4px; cursor: pointer;">
                        <div class="source-color-dot" style="background: ${safeColor(color)};"></div>
                        <div class="source-status-dot ${this.statusClass(source)}"
                             title="${esc(this.statusTitle(source))}"></div>
                        <div class="source-name">${esc(this.formatSourceName(source))}</div>
                    </div>
                    <div>
                        ${this.statusBadge(source)}
                        <span class="source-price">${hasPrice ? '$' + this.formatPrice(data.price) : '—'}</span>
                        <span class="price-change ${changeClass}">
                            ${hasPrice ? `${changeSymbol} ${Math.abs(data.changePercent).toFixed(3)}%` : ''}
                        </span>
                    </div>
                </div>
            `;
        }
        
        sourceList.innerHTML = html;
    }
    
    // Position of a source in the order the server sent it in meta.
    sourceOrderIndex(source) {
        const index = [...this.sourceMeta.keys()].indexOf(source);
        return index === -1 ? Number.MAX_SAFE_INTEGER : index;
    }

    updateSourcePrices() {
        for (const source of this.displayedSources()) {
            const data = this.sources.get(source);
            const sourceItem = document.querySelector(`[data-source="${source}"]`);
            if (sourceItem) {
                const priceElement = sourceItem.querySelector('.source-price');
                const changeElement = sourceItem.querySelector('.price-change');
                
                if (priceElement) {
                    priceElement.textContent = data ? `$${this.formatPrice(data.price)}` : '—';
                }
                
                if (changeElement) {
                    if (data) {
                        const changeClass = data.change >= 0 ? 'up' : 'down';
                        const changeSymbol = data.change >= 0 ? '↑' : '↓';
                        changeElement.className = `price-change ${changeClass}`;
                        changeElement.textContent = `${changeSymbol} ${Math.abs(data.changePercent).toFixed(3)}%`;
                    } else {
                        // No price for this source on this symbol. Leaving the
                        // old number here shows the previous symbol's change
                        // beside a "-" price.
                        changeElement.textContent = '';
                    }
                }

                const statusElement = sourceItem.querySelector('.source-status-dot');
                if (statusElement) {
                    statusElement.className = `source-status-dot ${this.statusClass(source)}`;
                    statusElement.title = this.statusTitle(source);
                }

                // The badge appears and disappears as a feed dies and recovers,
                // so it has to be refreshed on the cheap path too.
                const badge = sourceItem.querySelector('.source-badge');
                const wanted = this.statusBadge(source);
                if ((badge ? badge.outerHTML : '') !== wanted) {
                    if (badge) badge.remove();
                    if (wanted) {
                        const priceEl = sourceItem.querySelector('.source-price');
                        if (priceEl) priceEl.insertAdjacentHTML('beforebegin', wanted);
                    }
                }
            }
        }
    }

    updateChart() {
        if (!this.chartUpdatePending) {
            this.chartUpdatePending = true;
            requestAnimationFrame(() => {
                try {
                    this.performChartUpdate();
                } finally {
                    // Never latch the panel: a throw here would stop every
                    // further redraw while the UI still says "Connected".
                    this.chartUpdatePending = false;
                }
            });
        }
    }

    performChartUpdate() {
        if (!this.chart || this.priceHistory.size === 0) return;

        this.sourceMeta.forEach((meta, source) => {
            const series = this.chartSeries.get(source);
            if (!series) return;

            if (this.isSourceEnabled(source) && !this.absentSources.has(source)) {
                const history = this.priceHistory.get(source) || [];
                if (history.length > 0) {
                    // Convert data to TradingView format: { time: timestamp, value: price }
                    // A null price is a deliberate gap: TradingView renders a
                    // point with no value as whitespace and ends the line there.
                    const seriesData = history.map(([timestamp, price]) => (
                        price === null ? { time: timestamp } : { time: timestamp, value: price }
                    ));
                    
                    series.setData(seriesData);
                } else {
                    // Clear data if no history
                    series.setData([]);
                }
            } else {
                // Clear data for disabled sources
                series.setData([]);
            }
        });

        // Only auto-scroll to realtime if user hasn't manually interacted recently
        if (this.isAtRealtime && Date.now() - this.lastUserScrollTime > 3000) {
            this.chart.timeScale().scrollToRealTime();
        }
    }

    setupOpportunitiesTable() {
        // Setup table sorting
        const headers = document.querySelectorAll('.opportunities-table th.sortable');
        headers.forEach(header => {
            header.addEventListener('click', () => {
                const field = header.dataset.sort;
                if (this.currentSort.field === field) {
                    this.currentSort.direction = this.currentSort.direction === 'asc' ? 'desc' : 'asc';
                } else {
                    this.currentSort.field = field;
                    this.currentSort.direction = 'desc';
                }
                this.updateSortHeaders();
                this.updateOpportunitiesTable();
            });
        });
    }

    updateSortHeaders() {
        const headers = document.querySelectorAll('.opportunities-table th.sortable');
        headers.forEach(header => {
            header.classList.remove('sort-asc', 'sort-desc');
            if (header.dataset.sort === this.currentSort.field) {
                header.classList.add(this.currentSort.direction === 'asc' ? 'sort-asc' : 'sort-desc');
            }
        });
    }

    handleArbitrageOpportunity(opportunity) {
        // The identifier comes from the backend so the same event keeps one
        // identity across reconnects.
        this.arbitrageOpportunities.unshift(opportunity);
        
        if (this.arbitrageOpportunities.length > this.maxOpportunities) {
            this.arbitrageOpportunities = this.arbitrageOpportunities.slice(0, this.maxOpportunities);
        }

        this.updateOpportunitiesTable();
    }

    updateOpportunitiesTable() {
        if (!this.opportunitiesUpdatePending) {
            this.opportunitiesUpdatePending = true;
            setTimeout(() => {
                try {
                    this.performOpportunitiesTableUpdate();
                } finally {
                    // Never latch the panel: a throw here would stop every
                    // further redraw while the UI still says "Connected".
                    this.opportunitiesUpdatePending = false;
                }
            }, 250);
        }
    }
    
    performOpportunitiesTableUpdate() {
        const tbody = document.getElementById('opportunitiesTableBody');
        const stats = document.getElementById('opportunitiesStats');
        
        // Filter alerts by gross spread magnitude and enabled sources
        const filteredOpportunities = this.arbitrageOpportunities.filter(opp =>
            opp.spread_gross_pct >= this.minSpreadFilterPct &&
            this.isSourceEnabled(opp.buy_source) &&
            this.isSourceEnabled(opp.sell_source)
        );

        // Sort opportunities
        const sortedOpportunities = [...filteredOpportunities].sort((a, b) => {
            let aVal = a[this.currentSort.field];
            let bVal = b[this.currentSort.field];
            
            // Handle different data types
            if (this.currentSort.field === 'detected_at_ms') {
                aVal = new Date(aVal);
                bVal = new Date(bVal);
            } else if (typeof aVal === 'string') {
                aVal = aVal.toLowerCase();
                bVal = bVal.toLowerCase();
            }
            
            if (this.currentSort.direction === 'asc') {
                return aVal > bVal ? 1 : -1;
            } else {
                return aVal < bVal ? 1 : -1;
            }
        });

        // Update stats
        stats.textContent = `${filteredOpportunities.length} alerts`;

        if (sortedOpportunities.length === 0) {
            tbody.innerHTML = '<tr><td colspan="7" class="opportunities-empty">No alerts match current filters</td></tr>';
            return;
        }

        // Generate table rows
        let html = '';
        sortedOpportunities.forEach((opp) => {
            const isRecent = this.serverNowMs() - opp.detected_at_ms < 5000; // Fresh for 5 seconds
            const spreadClass = this.getSpreadMagnitudeClass(opp.spread_gross_pct);
            const timeStr = this.formatTime(opp.detected_at_ms);
            
            html += `
                <tr class="${isRecent ? 'fresh' : ''}" data-id="${esc(opp.id)}">
                    <td class="symbol-cell">${esc(opp.symbol)}</td>
                    <td class="spread-cell-value ${spreadClass}">${opp.spread_gross_pct.toFixed(3)}%</td>
                    <td class="source-cell">${esc(this.formatSourceName(opp.buy_source))}</td>
                    <td class="price-cell">$${this.formatPrice(opp.buy_price)}</td>
                    <td class="source-cell">${esc(this.formatSourceName(opp.sell_source))}</td>
                    <td class="price-cell">$${this.formatPrice(opp.sell_price)}</td>
                    <td class="time-cell">${timeStr}</td>
                </tr>
            `;
        });
        
        tbody.innerHTML = html;
    }

    // Magnitude band of a GROSS spread. Nothing here is profit: no fee, funding
    // or slippage has been deducted. See CLAUDE.md rule 2.
    getSpreadMagnitudeClass(spreadGrossPct) {
        if (spreadGrossPct >= 0.5) return 'high';
        if (spreadGrossPct >= 0.2) return 'medium';
        return 'low';
    }

    formatSourceName(source) {
        return this.sourceMeta.get(source)?.label
            || source.replace('_futures', '').replace('_', ' ').toUpperCase();
    }

    handleSpreadsUpdate(data) {
        if (data.symbol === this.currentSymbol) {
            // Batches only order what arrived together. Two ingestion goroutines
            // broadcast concurrently, so a message from an earlier batch can
            // still be older than what is already on screen.
            const current = this.currentSpreads.get(data.symbol);
            if (current && (data.server_time_ms ?? 0) < (current.serverTimeMs ?? 0)) {
                return;
            }
            this.currentSpreads.set(data.symbol, {
                groups: data.cross_venue_groups || [],
                basis: data.basis || [],
                oracleDeviation: data.oracle_deviation || [],
                excludedSources: data.excluded_sources || [],
                serverTimeMs: data.server_time_ms,
            });
            this.updateSpreadsMatrix();
        }
    }

    updateSpreadsMatrix() {
        if (!this.spreadsUpdatePending) {
            this.spreadsUpdatePending = true;
            setTimeout(() => {
                try {
                    this.performSpreadsMatrixUpdate();
                } finally {
                    // Never latch the panel: a throw here would stop every
                    // further redraw while the UI still says "Connected".
                    this.spreadsUpdatePending = false;
                }
            }, 300);
        }
    }
    
    performSpreadsMatrixUpdate() {
        const container = document.getElementById('spreadsMatrix');
        const data = this.currentSpreads.get(this.currentSymbol);

        if (!data) {
            container.innerHTML = '<div class="loading">Waiting for price data...</div>';
            return;
        }

        // One block per comparison group. A matrix is only ever built inside a
        // group, never across two, because sources in different groups are not
        // comparable - different market type or different quote asset.
        const blocks = (data.groups || [])
            .map(group => this.renderSpreadGroup(group))
            .filter(html => html !== '');

        // No group is a NORMAL state since step 1.2: a group needs two sources
        // that share a market type and a quote asset, so a venue left alone is
        // dropped as no_peer and no matrix remains. Returning here - which is
        // what this did - would hide the basis, the oracle deviation and above
        // all the excluded_sources list that says WHY the matrix is empty,
        // leaving the panel reading "waiting for data" while data is arriving.
        if (blocks.length === 0) {
            // Two different causes, and naming the wrong one sends the operator
            // looking in the wrong place: the backend sent no group at all, or
            // it sent groups whose every source the user has switched off.
            const message = (data.groups || []).length > 0
                ? 'Mọi nguồn trong các nhóm đang bị tắt ở bộ lọc nguồn phía trên.'
                : 'Không có nhóm nào so sánh được: mỗi nhóm cần ít nhất hai nguồn '
                  + 'cùng loại thị trường và cùng đồng quote. Lý do từng nguồn ở dưới.';
            blocks.push('<div class="spread-group"><div class="spread-group-note">'
                + message + '</div></div>');
        }

        if (data.basis.length > 0) {
            blocks.push(this.renderBasisBlock(data.basis));
        }
        if (data.oracleDeviation.length > 0) {
            blocks.push(this.renderOracleBlock(data.oracleDeviation));
        }
        if (data.excludedSources.length > 0) {
            blocks.push(this.renderExcludedSources(data.excludedSources));
        }

        container.innerHTML = blocks.join('');
    }

    // Basis is spot against perp on the SAME venue - the foundation of the
    // funding strategy, not a cross-venue opportunity.
    renderBasisBlock(basis) {
        const rows = basis.map(b => {
            const pct = b.basis_pct >= 0 ? `+${b.basis_pct.toFixed(3)}%` : `${b.basis_pct.toFixed(3)}%`;
            return `<div class="spread-basis-row">
                <span>${esc(this.formatSourceName(b.perp_source))} vs ${esc(this.formatSourceName(b.spot_source))}</span>
                <span class="${b.basis_pct >= 0 ? 'up' : 'down'}">${pct}</span>
            </div>`;
        }).join('');
        return `<div class="spread-group">
            <div class="spread-group-title">Basis (spot ↔ perp cùng sàn)</div>
            ${rows}
        </div>`;
    }

    // Oracle deviation is reference only: nothing here is executable.
    renderOracleBlock(deviations) {
        const rows = deviations.map(d => {
            const pct = d.deviation_pct >= 0 ? `+${d.deviation_pct.toFixed(3)}%` : `${d.deviation_pct.toFixed(3)}%`;
            // The oracle quotes in USD and most venues quote in USDT, so most of
            // these numbers carry the USD/USDT spread on top of the venue's own
            // drift. Rendering them all alike would present a mixed figure as a
            // clean one.
            const mismatch = d.quote_asset_mismatch
                ? ' <span class="spread-group-tag">lệch quote</span>'
                : '';
            return `<div class="spread-basis-row">
                <span>${esc(this.formatSourceName(d.source))}${mismatch}</span>
                <span class="${d.deviation_pct >= 0 ? 'up' : 'down'}">${pct}</span>
            </div>`;
        }).join('');
        return `<div class="spread-group">
            <div class="spread-group-title">Lệch so với oracle
                <span class="spread-group-tag">tham chiếu</span>
            </div>
            ${rows}
        </div>`;
    }

    // Renders the "why is this source missing" list the backend sends.
    renderExcludedSources(excluded) {
        const items = excluded
            .map(e => `<li>${esc(this.formatSourceName(e.source))}: ${esc(e.note_vi || e.reason)}</li>`)
            .join('');
        return `<div class="spread-group"><ul class="spread-excluded">${items}</ul></div>`;
    }

    renderSpreadGroup(group) {
        const sources = (group.sources || []).filter(source => this.isSourceEnabled(source));
        if (sources.length === 0) return '';

        let html = '<div class="spread-group">';
        html += `<div class="spread-group-title">${esc(group.label_vi || group.group_id)}`;
        if (!group.tradable) {
            html += ' <span class="spread-group-tag">tham chiếu</span>';
        }
        html += '</div>';
        if (group.note_vi) {
            html += `<div class="spread-group-note">${esc(group.note_vi)}</div>`;
        }

        html += `<div class="spreads-matrix" style="grid-template-columns: 60px repeat(${sources.length}, 1fr);">`;
        html += '<div class="spread-header"></div>';
        sources.forEach(sellSource => {
            html += `<div class="spread-header">${esc(this.getShortSourceName(sellSource))}</div>`;
        });

        sources.forEach(buySource => {
            html += `<div class="spread-row-header">${esc(this.getShortSourceName(buySource))}</div>`;
            sources.forEach(sellSource => {
                if (buySource === sellSource) {
                    html += '<div class="spread-cell neutral">-</div>';
                    return;
                }
                const cell = group.matrix?.[buySource]?.[sellSource];
                if (!cell) {
                    html += '<div class="spread-cell neutral">-</div>';
                    return;
                }
                html += this.renderSpreadCell(group, buySource, sellSource, cell);
            });
        });

        html += '</div></div>';
        return html;
    }

    // Renders the after-fee figure when the backend has one, and the gross
    // figure otherwise - always labelled, never silently mixed.
    renderSpreadCell(group, buySource, sellSource, cell) {
        const hasNet = typeof cell.spread_after_fees_pct === 'number';
        const shown = hasNet ? cell.spread_after_fees_pct : cell.spread_gross_pct;
        const text = shown >= 0 ? `+${shown.toFixed(2)}%` : `${shown.toFixed(2)}%`;

        // Sign colouring is just reading the number and stays as it always was.
        // The orange "opportunity" highlight is a claim that this is actionable,
        // so a group that still mixes market types never earns it.
        let cls = this.getSpreadClass(shown);
        if (!group.tradable && cls === 'opportunity') {
            cls = shown >= 0 ? 'positive' : 'negative';
        }

        const basis = hasNet ? 'đã trừ phí giao dịch' : 'THÔ, chưa trừ phí';
        const title = `Mua ${this.formatSourceName(buySource)} → Bán ${this.formatSourceName(sellSource)}: `
            + `${text} (${basis})`;

        return `<div class="spread-cell ${cls}" title="${esc(title)}">${text}</div>`;
    }

    getShortSourceName(source) {
        return this.sourceMeta.get(source)?.short_label
            || source.substring(0, 3).toUpperCase();
    }

    getSpreadClass(spread) {
        if (spread >= this.minSpreadFilterPct) {
            return 'opportunity';
        } else if (spread > 0) {
            return 'positive';
        } else {
            return 'negative';
        }
    }

    formatTime(timestamp) {
        const date = new Date(timestamp);
        const diffMs = Math.max(0, this.serverNowMs() - timestamp);
        
        if (diffMs < 60000) { // Less than 1 minute
            return Math.floor(diffMs / 1000) + 's';
        } else if (diffMs < 3600000) { // Less than 1 hour
            return Math.floor(diffMs / 60000) + 'm';
        } else {
            return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
        }
    }

    changeSymbol(newSymbol) {
        if (!newSymbol || newSymbol === this.currentSymbol) return;
        
        this.currentSymbol = newSymbol;
        this.sources.clear();
        // Absence is per symbol; carrying it over blanks healthy lines on the
        // new symbol until the next snapshot.
        this.absentSources.clear();
        this.priceHistory.clear();
        this.arbitrageOpportunities = [];
        
        // Clear all series data
        this.chartSeries.forEach(series => {
            series.setData([]);
        });

        this.updateChartTitle();
        this.updateSourceList();
        this.updateOpportunitiesTable();
        this.currentSpreads.clear();
        this.updateSpreadsMatrix();
        
        const symbolStatus = document.getElementById('symbolStatus');
        if (symbolStatus) symbolStatus.textContent = newSymbol;
        
        console.log(`Switched to symbol: ${newSymbol}`);
    }

    updateChartTitle() {
        const chartTitle = document.getElementById('chartTitle');
        chartTitle.textContent = this.currentSymbol
            ? `Price Chart - ${this.currentSymbol} (Live)`
            : 'Price Chart - đang chờ máy chủ';
    }


    

}

document.addEventListener('DOMContentLoaded', () => {
    window.scanner = new FuturesArbitrageScanner();
    console.log('Futures Arbitrage Scanner initialized');
});