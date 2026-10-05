// Dashboard rendering and interactions.
// Data is injected by cost_dashboard.py as window.dashboardData.
const dashboardData = window.dashboardData || {};

(function() {
    const dailyStats = dashboardData.dailyStats || [];

    // Collect all model names ordered by total cost (highest first)
    const modelTotals = {};
    dailyStats.forEach(d => {
        Object.entries(d.models).forEach(([m, c]) => {
            modelTotals[m] = (modelTotals[m] || 0) + c;
        });
    });
    const allModels = Object.keys(modelTotals).sort(
        (a, b) => modelTotals[b] - modelTotals[a]
    );

    // Distinct colour palette — one colour per model.
    // We cycle through a fixed set so the same model always
    // gets the same colour across page reloads.
    const PALETTE = [
        '#3fb950', // green  (matches accent-green)
        '#58a6ff', // blue
        '#a371f7', // purple
        '#d29922', // yellow
        '#f85149', // red
        '#39d353', // bright green
        '#79c0ff', // light blue
        '#ff7b72', // salmon
        '#ffa657', // orange
        '#56d364', // lime
        '#bc8cff', // lavender
        '#e3b341', // amber
    ];
    function modelColor(model, idx) {
        return PALETTE[idx % PALETTE.length];
    }

    // Only show the last 14 days by default; full history is
    // accessible via a toggle.
    const RECENT_DAYS = 14;
    let showAll = false;

    function getVisibleDays() {
        return showAll ? dailyStats : dailyStats.slice(-RECENT_DAYS);
    }

    function render() {
        const container = document.getElementById('daily-chart-content');
        const visible = getVisibleDays();
        if (!visible.length) {
            // Blank space is not a message. The chart is the biggest thing on
            // the page, so on a fresh install it is the first thing looked at.
            if (container) {
                container.innerHTML =
                    '<div class="empty-state">' +
                    '<strong>No spending recorded yet.</strong>' +
                    '<span>It appears once the scanner has read a session log. ' +
                    'The sources panel below reports what was looked for.</span>' +
                    '</div>';
            }
            return;
        }

        const maxCost = Math.max(...visible.map(d => d.cost), 0.0001);

        // Group days by YYYY-MM for monthly totals
        const monthTotals = {};
        visible.forEach(d => {
            const month = d.day.slice(0, 7);
            if (!monthTotals[month]) {
                monthTotals[month] = {cost: 0, models: {}};
            }
            monthTotals[month].cost += d.cost;
            Object.entries(d.models).forEach(([m, c]) => {
                monthTotals[month].models[m] =
                    (monthTotals[month].models[m] || 0) + c;
            });
        });

        let html = '';

        // Legend
        if (allModels.length > 0) {
            html += '<div class="daily-legend">';
            allModels.forEach((m, i) => {
                const color = modelColor(m, i);
                const shortName = m.length > 35
                    ? m.slice(0, 32) + '...' : m;
                html += `<span class="legend-item">
                    <span class="legend-dot" style="background:${color}"></span>
                    ${escapeHtml(shortName)}
                </span>`;
            });
            html += '</div>';
        }

        let prevMonth = null;

        visible.forEach(d => {
            const month = d.day.slice(0, 7);

            // Insert monthly total separator when month changes
            // (after we have seen all days of the previous month)
            if (prevMonth && month !== prevMonth) {
                html += renderMonthRow(prevMonth, monthTotals[prevMonth]);
            }
            prevMonth = month;

            // Stacked bar for this day
            let stackedSegments = '';
            allModels.forEach((m, i) => {
                const mCost = d.models[m] || 0;
                const mPct = (mCost / maxCost * 100);
                if (mPct < 0.01) return;
                stackedSegments += `<div class="bar-segment" style="width:${mPct.toFixed(2)}%;background:${modelColor(m, i)}" title="${escapeHtml(m)}: $${mCost.toFixed(4)}"></div>`;
            });

            html += `
                <div class="daily-bar">
                    <span class="date">${d.day}</span>
                    <div class="bar-wrapper">
                        <div class="bar-container stacked">
                            ${stackedSegments}
                        </div>
                    </div>
                    <span class="amount">$${d.cost.toFixed(2)}</span>
                </div>`;
        });

        // Monthly total for the last visible month
        if (prevMonth) {
            html += renderMonthRow(prevMonth, monthTotals[prevMonth]);
        }

        // Toggle button
        const totalDays = dailyStats.length;
        if (totalDays > RECENT_DAYS) {
            const label = showAll
                ? 'Show last 14 days'
                : `Show all ${totalDays} days`;
            html += `<div style="margin-top:12px;text-align:center">
                <button onclick="toggleDailyChart()" class="copy-btn">${label}</button>
            </div>`;
        }

        document.getElementById('daily-chart-content').innerHTML = html;
    }

    function renderMonthRow(month, mt) {
        const [year, mon] = month.split('-');
        const monthNames = [
            'January', 'February', 'March', 'April', 'May', 'June',
            'July', 'August', 'September', 'October', 'November', 'December'
        ];
        const label = `${monthNames[Number(mon) - 1] || mon} ${year}`;
        let segments = '';
        allModels.forEach((m, i) => {
            const mCost = mt.models[m] || 0;
            const mPct = mt.cost > 0 ? (mCost / mt.cost * 100) : 0;
            if (mPct < 0.01) return;
            segments += `<div class="bar-segment" style="width:${mPct.toFixed(2)}%;background:${modelColor(m, i)};opacity:0.55" title="${escapeHtml(m)}: $${mCost.toFixed(4)}"></div>`;
        });
        return `
            <div class="monthly-total-row">
                <span class="date monthly-label">${label}</span>
                <div class="bar-wrapper">
                    <div class="bar-container stacked">
                        ${segments}
                    </div>
                </div>
                <span class="amount monthly-amount">$${mt.cost.toFixed(2)}</span>
            </div>`;
    }

    window.toggleDailyChart = function() {
        showAll = !showAll;
        render();
    };

    render();
})();

const projects = dashboardData.projects || [];

function buildResumeCmd(agentCmd, cwd, sessionPath, sessionUid) {
    if (agentCmd === 'claude') {
        return 'cd "' + cwd + '" && claude --resume "' + sessionUid + '"';
    } else if (agentCmd === 'codex') {
        return 'cd "' + cwd + '" && codex --resume "' + sessionUid + '"';
    } else if (agentCmd === 'agy') {
        return 'cd "' + cwd + '" && agy --conversation "' + sessionUid + '"';
    } else {
        return 'cd "' + cwd + '" && ' + agentCmd + ' --session "' + sessionPath + '"';
    }
}

function formatDuration(seconds) {
    if (seconds < 60) {
        return Math.round(seconds) + 's';
    } else if (seconds < 3600) {
        const mins = Math.floor(seconds / 60);
        const secs = Math.round(seconds % 60);
        return mins + 'm' + secs.toString().padStart(2, '0') + 's';
    } else {
        const hours = Math.floor(seconds / 3600);
        const mins = Math.round((seconds % 3600) / 60);
        return hours + 'h' + mins.toString().padStart(2, '0') + 'm';
    }
}

let projectSort = { field: 'last_activity', asc: false };
let sessionsSort = { field: 'start', asc: false };

// Sessions are paged rather than all rendered: a few hundred sessions means a
// few hundred rows, each with resume commands and expandables, which is a
// multi-megabyte DOM for no benefit until someone scrolls to it.
const SESSIONS_PAGE_SIZE = 50;
let sessionsPage = 0;

function escapeHtml(text) {
    const div = document.createElement('div');
    div.textContent = text;
    return div.innerHTML;
}

// Column counts per table. The header row in the template and the cells each
// renderer emits are asserted against these by the Go test, so the two cannot
// drift apart again.
const SESSIONS_COLUMNS = 10;
const MODELS_COLUMNS = 11;
const TOOLS_COLUMNS = 6;
const PROJECTS_COLUMNS = 9;
const ACTIVITY_COLUMNS = 9;

// An empty tbody is indistinguishable from one that failed to render, and on a
// machine whose logs dashd cannot read every table is empty. That reader must
// not conclude there was no usage, so the row says what it means instead.
function emptyRow(columns, message) {
    return '<tr><td colspan="' + columns + '" class="empty-row">' +
        escapeHtml(message) + '</td></tr>';
}

function formatFullNumber(value) {
    const n = Number(value) || 0;
    return String(Math.round(n));
}

function trimOneDecimal(value) {
    return value.toFixed(1).replace(/\.0$/, '');
}

function formatCompactNumber(value) {
    const n = Number(value) || 0;
    const sign = n < 0 ? '-' : '';
    const abs = Math.abs(n);
    const units = [
        [1_000_000_000_000, 'T'],
        [1_000_000_000, 'B'],
        [1_000_000, 'M'],
        [1_000, 'k'],
    ];

    for (const [size, suffix] of units) {
        if (abs >= size) {
            return sign + trimOneDecimal(abs / size) + suffix;
        }
    }
    return sign + formatFullNumber(abs);
}

function displayNameFromPath(path) {
    const text = String(path || 'unknown').replace(/[\\/]+$/, '');
    const parts = text.split(/[\\/]+/);
    return parts[parts.length - 1] || text || 'unknown';
}

const TOKEN_DETAIL_FIELDS = [
    ['In', 'input_tokens'],
    ['Out', 'output_tokens'],
    ['Cache read', 'cache_read_tokens'],
    ['Cache write', 'cache_write_tokens'],
    ['Reasoning', 'reasoning_tokens'],
];

function tokenValue(item, field) {
    return Number(item?.[field] || 0);
}

function tokenTitle(item) {
    return [
        `Total: ${formatFullNumber(tokenValue(item, 'tokens'))}`,
        ...TOKEN_DETAIL_FIELDS.map(
            ([label, field]) => `${label}: ${formatFullNumber(tokenValue(item, field))}`
        ),
    ].join('\n');
}

function tokenDetailText(item, compact = false) {
    const formatter = compact ? formatCompactNumber : formatFullNumber;
    return TOKEN_DETAIL_FIELDS
        .map(([label, field]) => [label, tokenValue(item, field)])
        .filter(([, value]) => value > 0)
        .map(([label, value]) => `${label} ${formatter(value)}`)
        .join(' · ');
}

function tokenCellHtml(item) {
    return `<span class="token-cell" title="${escapeHtml(tokenTitle(item))}">${formatCompactNumber(tokenValue(item, 'tokens'))}</span>`;
}

// Column sort.
//
// Values are compared numerically whenever both sides parse as numbers, which
// is not a nicety: comparing them as text puts "1000" before "900", and every
// token count past six digits sorts as a string. Ties fall back to the row's
// original position, so equal values keep a stable order rather than shuffling
// between renders.
function compareValues(a, b) {
    const aNum = a !== null && a !== undefined && a !== '' && Number.isFinite(Number(a));
    const bNum = b !== null && b !== undefined && b !== '' && Number.isFinite(Number(b));
    if (aNum && bNum) {
        const na = Number(a);
        const nb = Number(b);
        return na < nb ? -1 : na > nb ? 1 : 0;
    }
    const sa = String(a == null ? '' : a).toLowerCase();
    const sb = String(b == null ? '' : b).toLowerCase();
    if (sa < sb) return -1;
    if (sa > sb) return 1;
    return 0;
}

function sortData(data, sort, accessor) {
    const get = accessor || (row => row[sort.field]);
    return data
        .map((row, idx) => ({row, idx, key: get(row)}))
        .sort((a, b) => {
            const cmp = compareValues(a.key, b.key);
            if (cmp !== 0) return sort.asc ? cmp : -cmp;
            return a.idx - b.idx;
        })
        .map(entry => entry.row);
}

// sessionSortValue is the sort key for one sessions-table column.
//
// Two of the columns are not plain fields: Project is the display form of the
// session's working directory, and Date is an epoch rather than the formatted
// string the cell shows.
function sessionSortValue(s, field) {
    switch (field) {
        case 'project':
            return s.cwd || '';
        case 'start':
            return Number(s.start) || 0;
        default:
            return s[field];
    }
}

function renderProjects() {
    const tbody = document.getElementById('projects-tbody');
    if (!tbody) return;
    const sorted = sortData(projects, projectSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(PROJECTS_COLUMNS, 'No projects in this view.');
        return;
    }
    tbody.innerHTML = sorted.map((p, idx) => {
        const displayName = displayNameFromPath(p.name);
        const shortName = displayName.length > 50 ? displayName.slice(0, 47) + '...' : displayName;
        const rowId = 'project-' + idx;

        // Build model breakdown HTML
        const modelRows = p.models.map(m => `
            <div class="model-item">
                <span class="model-name">${escapeHtml(m.name)}</span>
                <span class="model-stat" title="${formatFullNumber(m.messages)} msgs">${formatCompactNumber(m.messages)} msgs</span>
                <span class="model-stat token-wide" title="${escapeHtml(tokenTitle(m))}">${formatCompactNumber(m.tokens)} tok</span>
                <span class="model-stat token-detail-wide">${escapeHtml(tokenDetailText(m, true))}</span>
                <span class="model-stat" style="color: var(--accent-blue)">${(m.avg_tps || 0).toFixed(1)} tok/s</span>
                <span class="model-stat cost">$${m.cost.toFixed(2)}</span>
            </div>
        `).join('');

        // Build tool breakdown HTML
        const toolRows = (p.tools || []).map(t => `
            <div class="model-item">
                <span class="model-name" style="color: var(--accent-yellow)">${escapeHtml(t.name)}</span>
                <span class="model-stat" title="${formatFullNumber(t.calls)} calls">${formatCompactNumber(t.calls)} calls</span>
                <span class="model-stat" style="color: var(--accent-yellow)">${t.time_display}</span>
                <span class="model-stat">avg ${t.avg_time_display}</span>
                ${t.errors > 0 ? `<span class="model-stat" style="color: var(--accent-red)">${t.errors} errors</span>` : ''}
            </div>
        `).join('');

        return `
            <tr class="expandable-row" data-target="${rowId}" onclick="toggleProjectRow('${rowId}')">
                <td class="project-name" title="${escapeHtml(p.name)}"><span class="expand-icon">▶</span> ${escapeHtml(shortName)}</td>
                <td>${p.sessions}</td>
                <td title="${formatFullNumber(p.messages)}">${formatCompactNumber(p.messages)}</td>
                <td class="tokens">${tokenCellHtml(p)}</td>
                <td style="color: var(--accent-purple)">${p.llm_time_display}</td>
                <td style="color: var(--accent-yellow)">${p.tool_time_display}</td>
                <td style="color: var(--accent-blue)">${(p.avg_tps || 0).toFixed(1)}</td>
                <td class="cost">$${p.cost.toFixed(2)}</td>
                <td style="color: var(--text-secondary)">${p.last_activity_display}</td>
            </tr>
            <tr class="model-breakdown" id="${rowId}">
                <td colspan="${PROJECTS_COLUMNS}">
                    <div class="model-tree">
                        <div class="detail-line"><strong>Path:</strong> ${escapeHtml(p.name)}</div>
                        <div class="detail-line" title="${escapeHtml(tokenTitle(p))}"><strong>Tokens:</strong> ${formatCompactNumber(p.tokens)} ${tokenDetailText(p, true) ? `(${escapeHtml(tokenDetailText(p, true))})` : ''}</div>
                        <div style="font-weight: 600; margin-bottom: 8px; color: var(--text-secondary)">Models:</div>
                        ${modelRows || '<div style="color: var(--text-secondary)">No model data</div>'}
                        ${toolRows ? `<div style="font-weight: 600; margin: 12px 0 8px 0; color: var(--text-secondary)">Tools:</div>${toolRows}` : ''}
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

function toggleProjectRow(rowId) {
    const row = document.getElementById(rowId);
    const parentRow = document.querySelector('[data-target="' + rowId + '"]');
    row.classList.toggle('show');
    parentRow.classList.toggle('expanded');
}

function renderSessions() {
    const tbody = document.getElementById('sessions-tbody');
    if (!tbody) return;

    // Sessions arrive nested under their project; the table is flat, so they are
    // lifted out here. The project's agent command comes along because that is
    // what knows how to resume one of its sessions.
    const allSessions = [];
    projects.forEach(p => {
        p.sessions_list.forEach(s => {
            allSessions.push({...s, agent_cmd: p.agent_cmd});
        });
    });

    // Sort sessions using current sort state. sessionSortValue supplies the two
    // keys that are not plain fields.
    const sortedSessions = sortData(allSessions, sessionsSort,
        s => sessionSortValue(s, sessionsSort.field));

    const totalSessions = allSessions.length;
    document.getElementById('sessions-count').textContent = totalSessions + ' sessions';

    // Search and paging happen here rather than in the browser's own find, so a
    // few hundred sessions do not become a few hundred DOM rows. The list is
    // rebuilt on every sort anyway, so the state lives alongside it.
    const searchInput = document.getElementById('sessions-search');
    const shownEl = document.getElementById('sessions-shown');
    const prevBtn = document.getElementById('sessions-prev');
    const nextBtn = document.getElementById('sessions-next');
    const query = (searchInput?.value || '').trim().toLowerCase();

    const matches = s => {
        if (!query) return true;
        return [s.cwd, s.path, s.agent_cmd, s.title]
            .some(v => String(v || '').toLowerCase().includes(query));
    };
    const visibleSessions = sortedSessions.filter(matches);
    const totalPages = Math.max(1, Math.ceil(visibleSessions.length / SESSIONS_PAGE_SIZE));
    sessionsPage = Math.min(Math.max(sessionsPage, 0), totalPages - 1);
    const pageStart = sessionsPage * SESSIONS_PAGE_SIZE;
    const pageRows = visibleSessions.slice(pageStart, pageStart + SESSIONS_PAGE_SIZE);

    if (shownEl) {
        shownEl.textContent = query
            ? `${visibleSessions.length} of ${totalSessions} match \u201c${searchInput.value.trim()}\u201d`
            : `${totalSessions} sessions`;
    }
    if (prevBtn) prevBtn.disabled = sessionsPage === 0;
    if (nextBtn) nextBtn.disabled = sessionsPage >= totalPages - 1;

    if (!pageRows.length) {
        tbody.innerHTML = emptyRow(SESSIONS_COLUMNS, query
            ? 'No sessions match this filter.'
            : 'No sessions in this view.');
        return;
    }

    let html = '';

    pageRows.forEach(s => {
        const sessionUrl = '/session?uid=' + encodeURIComponent(s.uid);
        const resumePath = String(s.path || '').replace(/\\\\/g, '/');
        const resumeCmd = buildResumeCmd(s.agent_cmd, s.cwd, resumePath, s.uid);
        const sessionName = displayNameFromPath(s.cwd);
        const shortProject = sessionName.length > 40 ? sessionName.slice(0, 37) + '...' : sessionName;

        html += `
            <tr>
                <td class="project-name" title="${escapeHtml(s.cwd)}">${escapeHtml(shortProject)}</td>
                <td style="color: var(--text-secondary)">${s.start_display}</td>
                <td style="color: var(--text-secondary)">${s.duration_display}</td>
                <td style="color: var(--accent-purple)">${s.llm_time_display}</td>
                <td style="color: var(--accent-yellow)">${s.tool_time_display || '0s'}</td>
                <td style="color: var(--accent-blue)">${(s.avg_tps || 0).toFixed(1)}</td>
                <td title="${formatFullNumber(s.messages)}">${formatCompactNumber(s.messages)}</td>
                <td class="tokens">${tokenCellHtml(s)}</td>
                <td class="cost">$${s.cost.toFixed(2)}</td>
                <td>
                    <button onclick="copyResumeCommand(event, this.dataset.resumeCmd)" data-resume-cmd="${escapeHtml(resumeCmd)}" class="icon-btn" title="Copy resume command">Copy</button>
                    ${s.orphaned
                    ? '<span class="model-stat" title="This session\'s log is no longer on disk; its figures are kept" style="cursor:default">Log gone</span>'
                    : `<a href="${sessionUrl}" class="session-link" target="_blank" title="View full session">Open \u2192</a>`}
                </td>
            </tr>
        `;
    });

    tbody.innerHTML = html;
}

function copyResumeCommand(event, cmd) {
    const btn = event.target;

    function showSuccess() {
        const originalText = btn.textContent;
        btn.textContent = '✓';
        btn.style.color = 'var(--accent-green)';
        setTimeout(() => {
            btn.textContent = originalText;
            btn.style.color = '';
        }, 1500);
    }

    // Use clipboard API if available (HTTPS or localhost)
    if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(cmd).then(showSuccess).catch(err => {
            console.error('Failed to copy:', err);
        });
    } else {
        // Fallback for HTTP contexts
        const textArea = document.createElement('textarea');
        textArea.value = cmd;
        textArea.style.position = 'fixed';
        textArea.style.left = '-9999px';
        textArea.setAttribute('readonly', '');
        document.body.appendChild(textArea);
        textArea.select();
        try {
            document.execCommand('copy');
            showSuccess();
        } catch (err) {
            console.error('Fallback copy failed:', err);
        }
        document.body.removeChild(textArea);
    }
}

function setupSessionsToolbar() {
    const search = document.getElementById('sessions-search');
    const prev = document.getElementById('sessions-prev');
    const next = document.getElementById('sessions-next');
    if (!search && !prev && !next) return; // Section not rendered

    // Debounced because this re-renders the table: typing should not rebuild
    // hundreds of rows per keystroke.
    if (search) {
        let timer = null;
        search.addEventListener('input', () => {
            clearTimeout(timer);
            timer = setTimeout(() => {
                sessionsPage = 0;
                renderSessions();
            }, 150);
        });
    }
    prev?.addEventListener('click', () => {
        if (sessionsPage > 0) {
            sessionsPage -= 1;
            renderSessions();
        }
    });
    next?.addEventListener('click', () => {
        sessionsPage += 1;
        renderSessions();
    });
}

// Columns that read as ascending-first: names and dates. Everything else is a
// magnitude, where the useful first click is "largest first".
const ASCENDING_FIRST = new Set(['name', 'project', 'start']);

function setupSorting(tableId, sortState, renderFn) {
    document.querySelectorAll(`#${tableId} th[data-sort]`).forEach(th => {
        const field = th.dataset.sort;
        const activate = () => {
            if (sortState.field === field) {
                sortState.asc = !sortState.asc;
            } else {
                sortState.field = field;
                sortState.asc = ASCENDING_FIRST.has(field);
            }
            updateSortIcons(tableId, sortState);
            renderFn();
        };
        th.addEventListener('click', activate);
        th.setAttribute('tabindex', '0');
        th.setAttribute('role', 'button');
        th.addEventListener('keydown', e => {
            if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                activate();
            }
        });
    });
}

function updateSortIcons(tableId, sortState) {
    document.querySelectorAll(`#${tableId} th`).forEach(th => {
        const field = th.dataset.sort;
        const icon = th.querySelector('.sort-icon');
        if (!icon) return;
        if (field === sortState.field) {
            th.classList.add('sorted');
            icon.textContent = sortState.asc ? '▲' : '▼';
            th.setAttribute('aria-sort', sortState.asc ? 'ascending' : 'descending');
            th.title = 'Sorted ' + (sortState.asc ? 'ascending' : 'descending') +
                '; click to reverse';
        } else {
            th.classList.remove('sorted');
            icon.textContent = '▼';
            th.setAttribute('aria-sort', 'none');
            th.title = 'Sort by this column';
        }
    });
}

// ── Models table sorting ──────────────────────────────────────────────
const models = dashboardData.models || [];
let modelSort = { field: 'cost', asc: false };

function renderModels() {
    const tbody = document.getElementById('models-tbody');
    if (!tbody) return;
    const sorted = sortData(models, modelSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(MODELS_COLUMNS, 'No model calls in this view.');
        return;
    }

    tbody.innerHTML = sorted.map(m => {
        const modelClass = m.name.toLowerCase().includes('claude') ? 'model-claude' : 'model-other';
        // The tooltip lists every token column the row carries, which is what
        // the breakdown columns exist for: a total alone says nothing about
        // whether a cheap run was mostly cached.
        const breakdown = tokenTitle(m);

        return `
            <tr>
                <td><span class="model-tag ${modelClass}">${escapeHtml(m.name)}</span></td>
                <td title="${formatFullNumber(m.messages)}">${formatCompactNumber(m.messages)}</td>
                <td class="tokens" title="${escapeHtml(breakdown)}">${formatCompactNumber(m.tokens)}</td>
                <td class="tokens" title="${formatFullNumber(m.input_tokens)}">${formatCompactNumber(m.input_tokens)}</td>
                <td class="tokens" title="${formatFullNumber(m.output_tokens)}">${formatCompactNumber(m.output_tokens)}</td>
                <td class="tokens" title="${formatFullNumber(m.cache_read_tokens)}">${formatCompactNumber(m.cache_read_tokens)}</td>
                <td class="tokens" title="${formatFullNumber(m.cache_write_tokens)}">${formatCompactNumber(m.cache_write_tokens)}</td>
                <td class="tokens" title="${formatFullNumber(m.reasoning_tokens)}">${formatCompactNumber(m.reasoning_tokens)}</td>
                <td style="color: var(--accent-blue)">${(m.avg_tps || 0).toFixed(1)}</td>
                <td class="cost">$${m.cost.toFixed(2)}</td>
                <td>
                    <div class="bar-container" style="width: 100px; display: inline-block; vertical-align: middle;">
                        <div class="bar" style="width: ${m.pct}%"></div>
                    </div>
                    ${m.pct.toFixed(1)}%
                </td>
            </tr>
        `;
    }).join('');
}

// ── Tools table sorting ───────────────────────────────────────────────
const tools = dashboardData.tools || [];
let toolSort = { field: 'time', asc: false };

function renderTools() {
    const tbody = document.getElementById('tools-tbody');
    if (!tbody) return;
    const sorted = sortData(tools, toolSort);
    if (!sorted.length) {
        tbody.innerHTML = emptyRow(TOOLS_COLUMNS, 'No tool calls in this view.');
        return;
    }

    // The cost attributed to each tool is what the column header promises, so it
    // is what is rendered; the bar underneath is that tool's share of the total
    // attributed cost, not a share of wall time.
    const totalCostAttributed = tools.reduce((sum, t) => sum + (Number(t.cost) || 0), 0);

    tbody.innerHTML = sorted.map(t => {
        const errorStyle = t.errors > 0 ? 'color: var(--accent-red)' : 'color: var(--text-secondary)';
        const share = totalCostAttributed > 0
            ? (Number(t.cost) || 0) / totalCostAttributed * 100
            : 0;
        return `
            <tr>
                <td><span class="model-tag model-other">${escapeHtml(t.name)}</span></td>
                <td title="${formatFullNumber(t.calls)}">${formatCompactNumber(t.calls)}</td>
                <td style="color: var(--accent-yellow)">${t.time_display}</td>
                <td style="color: var(--text-secondary)">${t.avg_time_display}</td>
                <td style="${errorStyle}">${t.errors}</td>
                <td title="share of attributed cost: ${share.toFixed(1)}%">
                    <span class="cost">$${(Number(t.cost) || 0).toFixed(2)}</span>
                    <div class="bar-container" style="width: 100px; display: inline-block; vertical-align: middle;">
                        <div class="bar" style="width: ${share.toFixed(1)}%; background: var(--accent-green)"></div>
                    </div>
                </td>
            </tr>
        `;
    }).join('');
}

// Setup
setupSessionsToolbar();
setupSorting('projects-table', projectSort, renderProjects);
setupSorting('sessions-table', sessionsSort, renderSessions);
setupSorting('models-table', modelSort, renderModels);
setupSorting('tools-table', toolSort, renderTools);

// Initial render
renderProjects();
renderSessions();
renderModels();
renderTools();
updateSortIcons('projects-table', projectSort);
updateSortIcons('sessions-table', sessionsSort);
updateSortIcons('models-table', modelSort);
updateSortIcons('tools-table', toolSort);

// Activity & throughput: messages sent, tokens produced, and how quickly
// responses came back, over a rolling window that ends at the newest call.
//
// The series is fetched from /api/activity rather than read out of the page
// payload. That is what lets any window be exact: the server buckets the stored
// calls over the range asked for, so a chart of last March is as accurate as a
// chart of this afternoon even though March's logs are long gone.
(function() {
    const chartEl = document.getElementById('activity-chart');
    if (!chartEl) return; // Section not rendered

    const summaryEl = document.getElementById('activity-summary');
    const badgeEl = document.getElementById('activity-window-badge');
    const tbodyEl = document.getElementById('activity-tbody');
    const rangeEl = document.getElementById('activity-range');
    const metricEl = document.getElementById('activity-metric');
    const stepEl = document.getElementById('activity-step');
    const noteEl = document.querySelector('.activity-note');

    const DAY = 86400e3;
    const HOUR = 3600e3;

    // Every metric is derived from the same summed bucket fields, so switching
    // metric never changes what the window totals report.
    const METRICS = {
        messages: {
            label: 'Messages',
            color: '#58a6ff',
            value: b => b.messages,
            format: v => formatFullNumber(v),
        },
        outputTokens: {
            label: 'Output tokens',
            color: '#3fb950',
            value: b => b.output_tokens,
            format: v => formatCompactNumber(v),
        },
        totalTokens: {
            label: 'Total tokens',
            color: '#a371f7',
            value: b => b.total_tokens,
            format: v => formatCompactNumber(v),
        },
        throughput: {
            label: 'Throughput',
            color: '#3fb950',
            // Output over the time actually spent waiting on responses.
            // Weighting by seconds is the same rule the rest of the dashboard
            // uses for tokens/s, so the chart and the global card agree.
            value: b => (b.llm_seconds > 0 ? b.output_tokens / b.llm_seconds : 0),
            format: v => trimOneDecimal(v) + ' tok/s',
        },
        latency: {
            label: 'Response time',
            color: '#d29922',
            value: b => (b.messages > 0 ? b.llm_seconds / b.messages : 0),
            format: v => (v > 0 ? formatDuration(v) : '—'),
        },
        cost: {
            label: 'Cost',
            color: '#3fb950',
            value: b => b.cost,
            format: v => '$' + v.toFixed(2),
        },
    };

    // The controls live in the query string, like the filter bar, so a reload or
    // a shared link keeps the view.
    function readControlState() {
        const params = new URLSearchParams(window.location.search);
        const apply = (el, value, fallback) => {
            if (!el || value === null) return;
            const valid = Array.from(el.options).some(o => o.value === value);
            el.value = valid ? value : fallback;
        };
        apply(rangeEl, params.get('aw'), '86400');
        apply(metricEl, params.get('am'), 'messages');
        apply(stepEl, params.get('as'), 'auto');
    }

    function writeControlState() {
        if (!window.history || !window.history.replaceState) return;
        const params = new URLSearchParams(window.location.search);
        const put = (key, el, fallback) => {
            const value = el ? el.value : '';
            if (!value || value === fallback) params.delete(key);
            else params.set(key, value);
        };
        put('aw', rangeEl, '86400');
        put('am', metricEl, 'messages');
        put('as', stepEl, 'auto');
        const query = params.toString();
        window.history.replaceState(
            null, '', window.location.pathname + (query ? '?' + query : '')
        );
    }

    function currentMetric() {
        return METRICS[metricEl && metricEl.value] || METRICS.messages;
    }

    // ---------------------------------------------------------------- fetch

    let requestToken = 0;

    async function load() {
        const token = ++requestToken;
        const params = new URLSearchParams(window.location.search);
        // Only the axes that change this series are sent; the rest are already in
        // the query string the filter bar maintains.
        ['model', 'agent', 'project', 'date_from', 'date_to'].forEach(k => {
            const v = params.get(k);
            if (v) params.set(k, v);
        });
        params.set('range', rangeEl ? rangeEl.value : '86400');
        if (stepEl && stepEl.value && stepEl.value !== 'auto') {
            params.set('step', stepEl.value);
        } else {
            params.delete('step');
        }

        if (badgeEl) badgeEl.textContent = 'loading…';
        try {
            const resp = await fetch('/api/activity?' + params.toString(), {
                headers: { 'Accept': 'application/json' },
            });
            if (!resp.ok) throw new Error('status ' + resp.status);
            const data = await resp.json();
            // A slower earlier request must not overwrite a newer one.
            if (token !== requestToken) return;
            render(data);
        } catch (err) {
            if (token !== requestToken) return;
            if (chartEl) {
                chartEl.innerHTML =
                    '<div class="activity-empty">Could not load activity: ' +
                    escapeHtml(String(err)) + '</div>';
            }
            if (badgeEl) badgeEl.textContent = 'unavailable';
        }
    }

    // ---------------------------------------------------------------- render

    function render(data) {
        const buckets = data.buckets || [];
        const step = data.step * 1000;
        const metric = currentMetric();

        if (noteEl) {
            noteEl.style.display = data.oldest ? 'none' : 'block';
        }

        if (!buckets.length) {
            if (badgeEl) badgeEl.textContent = 'no activity';
            if (summaryEl) summaryEl.innerHTML = '';
            if (chartEl) {
                chartEl.innerHTML =
                    '<div class="activity-empty">No calls in this window.</div>';
            }
            if (tbodyEl) {
                tbodyEl.innerHTML = emptyRow(ACTIVITY_COLUMNS, 'No calls in this window.');
            }
            return;
        }

        const active = buckets.filter(b => b.messages > 0).length;
        if (badgeEl) {
            badgeEl.textContent = buckets.length + ' × ' + stepLabel(data.step) +
                ' · ' + active + ' active';
        }
        if (summaryEl) summaryEl.innerHTML = summarize(buckets, metric, data.step);
        chartEl.innerHTML = chart(buckets, metric, step, data);
        if (tbodyEl) tbodyEl.innerHTML = renderActivityTable(buckets, step, data);
    }

    function summarize(buckets, metric, stepSeconds) {
        const total = emptyBucket();
        buckets.forEach(b => {
            total.messages += b.messages;
            total.input_tokens += b.input_tokens;
            total.output_tokens += b.output_tokens;
            total.cache_read_tokens += b.cache_read_tokens;
            total.cache_write_tokens += b.cache_write_tokens;
            total.reasoning_tokens += b.reasoning_tokens;
            total.total_tokens += b.total_tokens;
            total.llm_seconds += b.llm_seconds;
            total.cost += b.cost;
            total.unpriced += b.unpriced || 0;
        });
        const values = buckets.map(metric.value);
        const peak = values.length ? Math.max(...values) : 0;
        const active = buckets.filter(b => b.messages > 0).length;

        const cards = [
            ['Messages sent', formatFullNumber(total.messages)],
            ['Output tokens', formatCompactNumber(total.output_tokens)],
            ['Total tokens', formatCompactNumber(total.total_tokens)],
            ['Throughput',
                total.llm_seconds > 0
                    ? trimOneDecimal(total.output_tokens / total.llm_seconds) + ' tok/s'
                    : '—'],
            ['Avg response',
                total.messages > 0 ? formatDuration(total.llm_seconds / total.messages) : '—'],
            ['Cost', '$' + total.cost.toFixed(2)],
            ['Peak ' + metric.label.toLowerCase(), peak > 0 ? metric.format(peak) : '—'],
            ['Active time',
                active > 0 ? formatDuration(active * stepSeconds) : '—'],
        ];
        // A model with no known price contributes tokens but no cost. Saying so
        // keeps a $0.00 from reading as "free".
        if (total.unpriced > 0) {
            cards.push(['Unpriced calls', formatFullNumber(total.unpriced)]);
        }

        return cards.map(([label, value]) =>
            '<div class="activity-summary-item">' +
            '<div class="label">' + escapeHtml(label) + '</div>' +
            '<div class="value">' + escapeHtml(value) + '</div></div>'
        ).join('');
    }

    function chart(buckets, metric, step, data) {
        // The server sends only non-empty buckets; gaps are filled here so an
        // idle stretch reads as idle rather than as missing data.
        // The server sends `t` in seconds; the axis works in milliseconds.
        // Normalising on the way in means a real bucket and a synthesised gap
        // are the same shape, rather than real buckets being labelled as 1970.
        // The server decides where every bucket boundary is; the browser only
        // fills the gaps between the buckets it was given.
        //
        // Re-deriving boundaries here was wrong: the server buckets on epoch
        // arithmetic, while flooring to a local midnight drifts by an hour across
        // a DST change. On such a day nothing matched, the chart came back empty,
        // and the table beneath it was full.
        const byStart = new Map();
        buckets.forEach(b => {
            const normalised = Object.assign({}, b, { t: b.t * 1000 });
            byStart.set(normalised.t, normalised);
        });
        const filled = [];
        if (byStart.size) {
            const first = byStart.keys().next().value;
            const last = Array.from(byStart.keys()).pop();
            for (let t = first; t <= last; t += step) {
                filled.push(byStart.get(t) || Object.assign(emptyBucket(), { t }));
            }
        }
        if (!filled.length) return '<div class="activity-empty">No activity.</div>';

        const values = filled.map(metric.value);
        const max = Math.max(...values, 0);
        // The newest bucket is the one the server is still filling.
        const current = filled[filled.length - 1].t;

        const columns = filled.map(bucket => {
            const value = metric.value(bucket);
            const height = max > 0 && value > 0 ? Math.max((value / max) * 100, 0.6) : 0;
            const classes = ['activity-col'];
            if (value <= 0) classes.push('empty');
            // The newest bucket is only partly elapsed, so it is marked rather
            // than silently shown as a full-width total.
            if (bucket.t === current) classes.push('partial');
            const lines = [bucketLabel(bucket.t, step)];
            if (bucket.t === current) lines.push('partial window, still filling');
            if (value > 0) {
                lines.push(metric.label + ': ' + metric.format(value));
                if (metric !== METRICS.messages) {
                    lines.push('Messages: ' + formatFullNumber(bucket.messages));
                }
                const tput = bucket.llm_seconds > 0
                    ? bucket.output_tokens / bucket.llm_seconds : 0;
                const resp = bucket.messages > 0
                    ? bucket.llm_seconds / bucket.messages : 0;
                lines.push('Output: ' + formatFullNumber(bucket.output_tokens));
                lines.push('Input: ' + formatFullNumber(bucket.input_tokens));
                lines.push('Throughput: ' +
                    (tput ? trimOneDecimal(tput) + ' tok/s' : '—'));
                lines.push('Avg response: ' + (resp ? formatDuration(resp) : '—'));
                lines.push('Cost: $' + bucket.cost.toFixed(4));
            } else {
                lines.push('No calls');
            }
            const color = value > 0 ? metric.color : '';
            return '<div class="' + classes.join(' ') + '" title="' +
                escapeHtml(lines.join('\n')) + '">' +
                '<div class="activity-col-fill" style="height:' + height.toFixed(2) +
                '%;' + (color ? 'background:' + color : '') + '"></div></div>';
        }).join('');

        // Label roughly a dozen buckets, however many there are.
        const every = Math.max(1, Math.ceil(filled.length / 12));
        const axis = filled.map((bucket, i) => {
            const show = i % every === 0 || i === filled.length - 1;
            return '<span class="activity-axis-tick">' +
                (show ? escapeHtml(axisLabel(bucket.t, step, data.from * 1000, data.to * 1000)) : '') + '</span>';
        }).join('');

        // An all-zero window has no meaningful top of scale, and inventing one
        // would put a number on the axis that is not in the data.
        const ticks = max > 0
            ? [1, 0.5, 0].map(f => {
                const bottom = f * 100;
                const label = f === 0 ? '0' : metric.format(max * f);
                return '<span class="activity-y-tick" style="bottom:' + bottom + '%">' +
                    escapeHtml(label) + '</span>';
            }).join('')
            : '<span class="activity-y-tick" style="bottom:0%">0</span>';
        const gridlines = max > 0
            ? [50, 0].map(f =>
                '<div class="activity-gridline" style="bottom:' + f + '%"></div>').join('')
            : '';

        return '<div class="activity-plot">' +
            '<div class="activity-yaxis">' + ticks + '</div>' +
            '<div class="activity-plotarea">' + gridlines +
            '<div class="activity-columns">' + columns + '</div></div></div>' +
            '<div class="activity-axis">' + axis + '</div>';
    }

    function renderActivityTable(buckets, step, data) {
        const current = buckets.length
            ? buckets[buckets.length - 1].t * 1000
            : 0;
        const rows = [...buckets].reverse().map(bucket => {
            // Seconds to milliseconds, matching what the chart shows.
            bucket = Object.assign({}, bucket, { t: bucket.t * 1000 });
            const tput = bucket.llm_seconds > 0
                ? bucket.output_tokens / bucket.llm_seconds : 0;
            const resp = bucket.messages > 0
                ? bucket.llm_seconds / bucket.messages : 0;
            const partial = bucket.t === current;
            return '<tr>' +
                '<td class="bucket-label' + (partial ? ' current' : '') + '">' +
                    escapeHtml(bucketLabel(bucket.t, step)) +
                    (partial ? ' (partial)' : '') + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.messages) + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.output_tokens) + '</td>' +
                '<td class="numeric">' + formatFullNumber(bucket.input_tokens) + '</td>' +
                '<td class="numeric" title="' +
                    escapeHtml(formatFullNumber(bucket.total_tokens)) + '">' +
                    formatCompactNumber(bucket.total_tokens) + '</td>' +
                '<td class="numeric">' + (tput ? trimOneDecimal(tput) : '—') + '</td>' +
                '<td class="numeric">' + (resp ? formatDuration(resp) : '—') + '</td>' +
                '<td class="numeric">' + formatDuration(bucket.llm_seconds) + '</td>' +
                '<td class="numeric">$' + bucket.cost.toFixed(4) + '</td></tr>';
        }).join('');
        return rows || emptyRow(ACTIVITY_COLUMNS, 'No activity in this window.');
    }

    // ---------------------------------------------------------------- helpers

    function emptyBucket() {
        return {
            t: 0, messages: 0, input_tokens: 0, output_tokens: 0,
            cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0,
            total_tokens: 0, llm_seconds: 0, cost: 0, unpriced: 0,
        };
    }

    function stepLabel(seconds) {
        if (seconds >= 86400) return trimOneDecimal(seconds / 86400) + 'd';
        if (seconds >= 3600) return trimOneDecimal(seconds / 3600) + 'h';
        if (seconds >= 60) return trimOneDecimal(seconds / 60) + 'm';
        return seconds + 's';
    }

    function axisLabel(ms, step, from, to) {
        const d = new Date(ms);
        if (step >= DAY) {
            return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
        }
        // An hourly tick is labelled with the day as well as the hour once the
        // window spans one. Without it a 24-hour view whose labels are all "03"
        // reads as a rendering fault rather than as the clock going round.
        const spansDays = to - from > 20 * HOUR;
        if (step >= HOUR && spansDays) {
            return d.toLocaleDateString(undefined, { weekday: 'short' }) + ' ' +
                d.toLocaleTimeString(undefined, { hour: '2-digit' });
        }
        return d.toLocaleTimeString(undefined, {
            hour: '2-digit', minute: step < HOUR ? '2-digit' : undefined,
        });
    }

    function bucketLabel(ms, step) {
        const start = new Date(ms);
        if (step >= DAY) {
            return start.toLocaleDateString(undefined, {
                weekday: 'short', month: 'short', day: 'numeric',
            });
        }
        const end = new Date(ms + step);
        const time = t => t.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
        return time(start) + ' – ' + time(end);
    }

    // ---------------------------------------------------------------- wiring

    [rangeEl, metricEl, stepEl].forEach(el => {
        if (el) {
            el.addEventListener('change', () => {
                writeControlState();
                load();
            });
        }
    });

    readControlState();
    load();
})();
