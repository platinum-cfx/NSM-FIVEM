// ============================================================================
// NSM Platform & 1:1 NSM.exe Server Browser Controller
// ============================================================================
(function () {
  'use strict';

  try {
    if (window.opener && !window.opener.closed && window.opener !== window && window.opener.location && window.opener.location.origin === window.location.origin) {
      window.opener.close();
    }
  } catch (e) {}

  var COLOR_MAP = {
    '0': '#f0f4f8',
    '1': '#ff5252',
    '2': '#4cd964',
    '3': '#ffd60a',
    '4': '#388bff',
    '5': '#32ade6',
    '6': '#bf5af2',
    '7': '#f0f4f8',
    '8': '#ff9500',
    '9': '#98a2b3'
  };

  function escapeHtml(str) {
    return String(str || '')
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function colorizeHostname(raw) {
    if (!raw) return 'NSM Server';
    var parts = String(raw).split(/(\^[0-9])/g);
    var currentColor = null;
    var html = '';
    for (var i = 0; i < parts.length; i++) {
      var token = parts[i];
      if (!token) continue;
      if (token.length === 2 && token.charAt(0) === '^' && COLOR_MAP[token.charAt(1)]) {
        currentColor = COLOR_MAP[token.charAt(1)];
      } else {
        var safe = escapeHtml(token);
        if (currentColor) {
          html += '<span style="color:' + currentColor + '">' + safe + '</span>';
        } else {
          html += safe;
        }
      }
    }
    return html || escapeHtml(raw);
  }

  function applyAllColorize() {
    var nodes = document.querySelectorAll('[data-colorize]');
    for (var i = 0; i < nodes.length; i++) {
      var raw = nodes[i].getAttribute('data-colorize');
      if (raw) {
        nodes[i].innerHTML = colorizeHostname(raw);
      }
    }
  }

  // ---- LocalStorage helpers for Favorites & History ----
  function getStoredSet(key) {
    try {
      var raw = localStorage.getItem(key);
      var arr = raw ? JSON.parse(raw) : [];
      var map = {};
      if (Array.isArray(arr)) {
        for (var i = 0; i < arr.length; i++) map[arr[i]] = true;
      }
      return map;
    } catch (e) {
      return {};
    }
  }

  function saveStoredSet(key, map) {
    try {
      localStorage.setItem(key, JSON.stringify(Object.keys(map)));
    } catch (e) {}
  }

  var favSet = getStoredSet('nsm_favorite_servers');
  var histSet = getStoredSet('nsm_history_servers');

  function recordHistory(ep) {
    if (!ep) return;
    histSet[ep] = true;
    saveStoredSet('nsm_history_servers', histSet);
    updateTabCounters();
  }

  function toggleFavorite(ep) {
    if (!ep) return;
    if (favSet[ep]) {
      delete favSet[ep];
    } else {
      favSet[ep] = true;
    }
    saveStoredSet('nsm_favorite_servers', favSet);
    syncFavoriteButtons();
    updateTabCounters();
    filterAndSortRows();
  }

  function syncFavoriteButtons() {
    var btns = document.querySelectorAll('[data-fav-ep]');
    for (var i = 0; i < btns.length; i++) {
      var ep = btns[i].getAttribute('data-fav-ep');
      var isFav = !!favSet[ep];
      btns[i].classList.toggle('active', isFav);
      var row = btns[i].closest('.nsm-server-row');
      if (row) {
        var favWrap = row.querySelector('.nsm-srv-fav');
        if (favWrap) favWrap.classList.toggle('is-fav', isFav);
      }
    }
  }

  function updateTabCounters() {
    var rows = document.querySelectorAll('.nsm-server-row');
    var allEl = document.getElementById('nsm-tab-count-all');
    var favEl = document.getElementById('nsm-tab-count-favorites');
    var histEl = document.getElementById('nsm-tab-count-history');
    if (allEl) allEl.textContent = String(rows.length);
    var favCount = 0;
    var histCount = 0;
    for (var i = 0; i < rows.length; i++) {
      var ep = rows[i].getAttribute('data-ep');
      if (favSet[ep]) favCount++;
      if (histSet[ep]) histCount++;
    }
    if (favEl) favEl.textContent = String(favCount);
    if (histEl) histEl.textContent = String(histCount);
  }

  // ---- Connect helper ----
  function connectToServer(ep) {
    if (!ep) return;
    recordHistory(ep);
    try {
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText('connect ' + ep).catch(function () {});
      }
    } catch (e) {}
    var proto = 'fiv' + 'em://connect/' + ep;
    window.location.href = proto;
  }

  // ---- Upvote / Boost & Owner Featured Server helpers ----
  function upvoteServerByEp(ep) {
    if (!ep) return;
    fetch('/api/servers/upvote', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ endpoint: ep })
    })
      .then(function (r) {
        if (r.status === 401) {
          window.location.href = '/login?next=' + encodeURIComponent(window.location.pathname);
          return null;
        }
        return r.json();
      })
      .then(function (data) {
        if (!data || typeof data.upvotes !== 'number') return;
        var nodes = document.querySelectorAll('[data-upvote-count="' + ep + '"]');
        for (var i = 0; i < nodes.length; i++) {
          nodes[i].textContent = String(data.upvotes);
        }
        var row = document.querySelector('.nsm-server-row[data-ep="' + ep + '"]');
        if (row) {
          row.setAttribute('data-upvotes', String(data.upvotes));
        }
        var inspUp = document.getElementById('nsm-insp-upvotes');
        var inspEp = document.getElementById('nsm-insp-ep');
        if (inspUp && inspEp && inspEp.textContent === ep) {
          inspUp.textContent = String(data.upvotes);
        }
        updateTabCounters();
        filterAndSortRows();
      })
      .catch(function () {});
  }

  function renderFeaturedServersBoxFromDOM() {
    var listEl = document.getElementById('nsm-featured-servers-list');
    if (!listEl) return;
    var rows = getRowsArray();
    var featRows = [];
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute('data-featured') === '1') {
        featRows.push(rows[i]);
      }
    }
    if (featRows.length === 0) {
      listEl.innerHTML = '<p id="nsm-featured-empty" style="font-size:0.78rem;color:rgba(240,244,248,0.55);margin:2px 0">Official featured NSM servers pinned by nta~ appear here.</p>';
      return;
    }
    var html = '';
    for (var j = 0; j < featRows.length; j++) {
      var r = featRows[j];
      var ep = r.getAttribute('data-ep') || '';
      var rawName = r.getAttribute('data-raw-name') || r.getAttribute('data-name') || ep;
      var clients = r.getAttribute('data-clients') || '0';
      var maxClients = r.getAttribute('data-maxclients') || '48';
      var upv = r.getAttribute('data-upvotes') || '0';
      html += '<div class="nsm-featured-card" data-featured-card="' + escapeHtml(ep) + '" style="display:flex;align-items:center;justify-content:space-between;gap:10px;padding:8px 10px;border-radius:8px;background:rgba(15,23,42,0.75);border:1px solid rgba(245,158,11,0.35);cursor:pointer">'
        + '<div style="min-width:0;flex:1">'
        + '<span style="color:#fff;font-weight:700;font-size:0.86rem;display:block;white-space:nowrap;overflow:hidden;text-overflow:ellipsis">' + colorizeHostname(rawName) + '</span>'
        + '<div style="font-size:0.73rem;color:rgba(240,244,248,0.65);display:flex;gap:10px;margin-top:2px">'
        + '<span>' + escapeHtml(clients) + '/' + escapeHtml(maxClients) + ' players</span>'
        + '<span style="color:#00f8b9">&#9889; <span data-upvote-count="' + escapeHtml(ep) + '">' + escapeHtml(upv) + '</span></span>'
        + '</div></div>'
        + '<button type="button" class="nsm-connect-btn" data-connect-ep="' + escapeHtml(ep) + '" style="padding:5px 10px;font-size:0.75rem">Connect</button>'
        + '</div>';
    }
    listEl.innerHTML = html;
  }

  function featureServerByEp(ep) {
    if (!ep) return;
    fetch('/api/servers/feature', {
      method: 'POST',
      credentials: 'same-origin',
      headers: {
        'Accept': 'application/json',
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ endpoint: ep })
    })
      .then(function (r) { return r.json(); })
      .then(function (data) {
        if (!data || !data.ok) return;
        var isFeat = !!data.featured;
        var row = document.querySelector('.nsm-server-row[data-ep="' + ep + '"]');
        if (row) {
          row.setAttribute('data-featured', isFeat ? '1' : '0');
        }
        var badges = document.querySelectorAll('[data-featured-badge="' + ep + '"]');
        for (var b = 0; b < badges.length; b++) {
          badges[b].style.display = isFeat ? '' : 'none';
        }
        var btns = document.querySelectorAll('[data-feature-ep="' + ep + '"]');
        for (var i = 0; i < btns.length; i++) {
          btns[i].innerHTML = '&#9733; ' + (isFeat ? 'Featured' : 'Feature');
          btns[i].style.background = isFeat ? 'rgba(245,158,11,0.24)' : '';
          btns[i].style.borderColor = isFeat ? '#f59e0b' : '';
          btns[i].style.color = isFeat ? '#fbbf24' : '';
        }
        var inspFeatLbl = document.getElementById('nsm-insp-feature-label');
        var inspEp = document.getElementById('nsm-insp-ep');
        if (inspFeatLbl && inspEp && inspEp.textContent === ep) {
          inspFeatLbl.innerHTML = '&#9733; ' + (isFeat ? 'Featured' : 'Feature');
        }
        renderFeaturedServersBoxFromDOM();
        filterAndSortRows();
      })
      .catch(function () {});
  }

  // ---- State for 1:1 NSM.exe Server Browser ----
  var state = {
    listType: 'all',
    searchText: '',
    hideEmpty: false,
    hideFull: false,
    sortBy: 'players',
    sortDir: -1,
    tagFacets: {},
    localeFacets: {},
    activeFacetTab: 'tags',
    facetSearch: '',
    selectedEp: ''
  };

  function getRowsArray() {
    return Array.prototype.slice.call(document.querySelectorAll('.nsm-server-row'));
  }

  function updateInspectorFromRow(row) {
    if (!row) return;
    var ep = row.getAttribute('data-ep') || '127.0.0.1:30120';
    state.selectedEp = ep;
    var allRows = getRowsArray();
    for (var i = 0; i < allRows.length; i++) {
      allRows[i].classList.toggle('selected', allRows[i] === row);
    }

    var rawName = row.getAttribute('data-raw-name') || row.getAttribute('data-name') || ep;
    var cleanName = row.getAttribute('data-name') || ep;
    var desc = row.getAttribute('data-desc') || row.getAttribute('data-gametype') || 'Live NSM Dedicated Server';
    var clients = row.getAttribute('data-clients') || '0';
    var maxClients = row.getAttribute('data-maxclients') || '48';
    var upvotes = row.getAttribute('data-upvotes') || '0';
    var resCount = row.getAttribute('data-resources') || '0';
    var locale = row.getAttribute('data-locale') || 'en-US';
    var onesync = row.getAttribute('data-onesync') || 'enabled';
    var tagsRaw = row.getAttribute('data-tags') || '';

    var titleEl = document.getElementById('nsm-insp-title');
    var descEl = document.getElementById('nsm-insp-desc');
    var initEl = document.getElementById('nsm-insp-initial');
    var playersEl = document.getElementById('nsm-insp-players');
    var epEl = document.getElementById('nsm-insp-ep');
    var resEl = document.getElementById('nsm-insp-res');
    var locEl = document.getElementById('nsm-insp-locale');
    var f8El = document.getElementById('nsm-insp-f8');
    var joinEl = document.getElementById('nsm-insp-join');
    var upvEl = document.getElementById('nsm-insp-upvotes');
    var featLblEl = document.getElementById('nsm-insp-feature-label');
    var fullLink = document.getElementById('nsm-insp-fullpage');
    var tagsEl = document.getElementById('nsm-insp-tags');
    var isFeat = row.getAttribute('data-featured') === '1';

    if (titleEl) titleEl.innerHTML = colorizeHostname(rawName);
    if (descEl) descEl.textContent = desc;
    if (initEl) initEl.textContent = cleanName.charAt(0).toUpperCase() || 'S';
    if (playersEl) playersEl.textContent = clients + ' / ' + maxClients;
    var jc = row.getAttribute('data-join-code') || '';
    var joinRef = jc || ep;
    if (epEl) epEl.textContent = joinRef;
    if (resEl) resEl.textContent = resCount + ' loaded';
    if (locEl) locEl.textContent = locale + (onesync ? ' • OneSync' : '');
    if (f8El) f8El.textContent = 'connect ' + joinRef;
    if (joinEl) joinEl.textContent = 'https://nsmultiplayer.com/join/' + joinRef;
    if (upvEl) upvEl.textContent = upvotes;
    if (featLblEl) featLblEl.innerHTML = '&#9733; ' + (isFeat ? 'Featured' : 'Feature');
    if (fullLink) fullLink.setAttribute('href', '/join/' + encodeURIComponent(joinRef));

    if (tagsEl) {
      var tags = tagsRaw ? tagsRaw.split(',') : [];
      if (tags.length === 0) {
        tagsEl.innerHTML = '<span class="nsm-loaf">nsm</span><span class="nsm-loaf">afx</span>';
      } else {
        var th = '';
        for (var t = 0; t < tags.length; t++) {
          if (tags[t]) th += '<span class="nsm-loaf">' + escapeHtml(tags[t]) + '</span>';
        }
        tagsEl.innerHTML = th;
      }
    }
  }

  function buildFacetsFromDOM() {
    var rows = getRowsArray();
    var tagCounts = {};
    var localeCounts = {};

    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var tagsStr = r.getAttribute('data-tags') || '';
      if (tagsStr) {
        var tags = tagsStr.split(',');
        for (var j = 0; j < tags.length; j++) {
          var t = tags[j].trim();
          if (t) tagCounts[t] = (tagCounts[t] || 0) + 1;
        }
      }
      var loc = (r.getAttribute('data-locale') || 'en-US').trim();
      var country = (r.getAttribute('data-country') || 'US').trim();
      var locKey = loc + ' (' + country + ')';
      localeCounts[locKey] = (localeCounts[locKey] || 0) + 1;
    }

    renderFacetList('nsm-facet-tags-list', tagCounts, state.tagFacets, 'tag');
    renderFacetList('nsm-facet-locales-list', localeCounts, state.localeFacets, 'locale');
    renderSearchChips();
    updateFilterBadge();
  }

  function renderFacetList(containerId, countsMap, activeMap, kind) {
    var el = document.getElementById(containerId);
    if (!el) return;
    var keys = Object.keys(countsMap).sort(function (a, b) {
      return countsMap[b] - countsMap[a] || a.localeCompare(b);
    });
    var q = state.facetSearch.toLowerCase();
    var html = '';
    var shown = 0;
    for (var i = 0; i < keys.length; i++) {
      var k = keys[i];
      if (q && k.toLowerCase().indexOf(q) === -1) continue;
      shown++;
      var mode = activeMap[k] || 0;
      var cls = 'nsm-faucet';
      var prefix = '';
      if (mode === 1) {
        cls += ' inclusive';
        prefix = '+ ';
      } else if (mode === -1) {
        cls += ' exclusive';
        prefix = '− ';
      }
      html += '<div class="' + cls + '" data-faucet-kind="' + kind + '" data-faucet-key="' + escapeHtml(k) + '">' +
        '<span class="left">' + prefix + escapeHtml(k) + '</span>' +
        '<span class="right">' + countsMap[k] + '</span>' +
        '</div>';
    }
    if (!shown) {
      html = '<div style="padding:14px;text-align:center;color:rgba(240,244,248,0.45);font-size:0.8rem">No matching ' + kind + 's</div>';
    }
    el.innerHTML = html;
  }

  function renderSearchChips() {
    var wrap = document.getElementById('nsm-search-chips');
    if (!wrap) return;
    var html = '';
    Object.keys(state.tagFacets).forEach(function (t) {
      var m = state.tagFacets[t];
      if (!m) return;
      var cls = m === -1 ? 'nsm-search-chip exclusive' : 'nsm-search-chip';
      html += '<span class="' + cls + '" data-remove-tag="' + escapeHtml(t) + '">' +
        (m === -1 ? '~' : '') + 'tag:' + escapeHtml(t) + ' <span>&times;</span></span>';
    });
    Object.keys(state.localeFacets).forEach(function (l) {
      var m = state.localeFacets[l];
      if (!m) return;
      var cls = m === -1 ? 'nsm-search-chip exclusive' : 'nsm-search-chip';
      html += '<span class="' + cls + '" data-remove-locale="' + escapeHtml(l) + '">' +
        (m === -1 ? '~' : '') + 'loc:' + escapeHtml(l) + ' <span>&times;</span></span>';
    });
    wrap.innerHTML = html;
  }

  function updateFilterBadge() {
    var badge = document.getElementById('nsm-filter-badge');
    var btn = document.getElementById('nsm-filter-btn');
    if (!badge) return;
    var count = 0;
    if (state.hideEmpty) count++;
    if (state.hideFull) count++;
    Object.keys(state.tagFacets).forEach(function (k) { if (state.tagFacets[k]) count++; });
    Object.keys(state.localeFacets).forEach(function (k) { if (state.localeFacets[k]) count++; });
    if (count > 0) {
      badge.style.display = 'inline-block';
      badge.textContent = String(count);
      if (btn) btn.classList.add('active');
    } else {
      badge.style.display = 'none';
      if (btn) btn.classList.remove('active');
    }
  }

  function filterAndSortRows() {
    var listBody = document.getElementById('nsm-server-list-body');
    if (!listBody) return;
    var rows = getRowsArray();
    var q = state.searchText.trim().toLowerCase();

    // Direct Connect IP:Port popover detection
    var dcPop = document.getElementById('nsm-direct-connect-popover');
    var dcLbl = document.getElementById('nsm-dc-label');
    if (dcPop) {
      if (/^[a-z0-9.\-]+:\d{2,5}$/i.test(state.searchText.trim())) {
        dcPop.style.display = 'block';
        if (dcLbl) dcLbl.innerHTML = 'Direct Connect to <code>' + escapeHtml(state.searchText.trim()) + '</code>';
      } else {
        dcPop.style.display = 'none';
      }
    }

    var incTags = [];
    var excTags = [];
    Object.keys(state.tagFacets).forEach(function (t) {
      if (state.tagFacets[t] === 1) incTags.push(t.toLowerCase());
      if (state.tagFacets[t] === -1) excTags.push(t.toLowerCase());
    });

    var incLocs = [];
    var excLocs = [];
    Object.keys(state.localeFacets).forEach(function (l) {
      if (state.localeFacets[l] === 1) incLocs.push(l);
      if (state.localeFacets[l] === -1) excLocs.push(l);
    });

    var visibleRows = [];
    var firstVisible = null;

    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var ep = r.getAttribute('data-ep') || '';
      var name = (r.getAttribute('data-name') || '').toLowerCase();
      var desc = (r.getAttribute('data-desc') || '').toLowerCase();
      var gametype = (r.getAttribute('data-gametype') || '').toLowerCase();
      var mapname = (r.getAttribute('data-mapname') || '').toLowerCase();
      var tagsStr = (r.getAttribute('data-tags') || '').toLowerCase();
      var clients = parseInt(r.getAttribute('data-clients') || '0', 10);
      var maxClients = parseInt(r.getAttribute('data-maxclients') || '48', 10);
      var upvotes = parseInt(r.getAttribute('data-upvotes') || '0', 10);
      var burst = parseInt(r.getAttribute('data-burst') || '0', 10);
      var loc = (r.getAttribute('data-locale') || 'en-US').trim();
      var country = (r.getAttribute('data-country') || 'US').trim();
      var locKey = loc + ' (' + country + ')';

      var show = true;
      if (state.listType === 'favorites' && !favSet[ep]) show = false;
      if (state.listType === 'history' && !histSet[ep]) show = false;
      if (state.listType === 'supporters' && upvotes <= 0 && burst <= 0) show = false;

      if (show && state.hideEmpty && clients <= 0) show = false;
      if (show && state.hideFull && maxClients > 0 && clients >= maxClients) show = false;

      if (show && q) {
        var hay = name + ' ' + desc + ' ' + gametype + ' ' + mapname + ' ' + tagsStr + ' ' + ep.toLowerCase() + ' ' + country.toLowerCase();
        if (hay.indexOf(q) === -1) show = false;
      }

      if (show && (incTags.length || excTags.length)) {
        var rowTags = tagsStr ? tagsStr.split(',') : [];
        for (var it = 0; it < incTags.length; it++) {
          if (rowTags.indexOf(incTags[it]) === -1) { show = false; break; }
        }
        if (show) {
          for (var et = 0; et < excTags.length; et++) {
            if (rowTags.indexOf(excTags[et]) !== -1) { show = false; break; }
          }
        }
      }

      if (show && (incLocs.length || excLocs.length)) {
        if (incLocs.length && incLocs.indexOf(locKey) === -1) show = false;
        if (show && excLocs.indexOf(locKey) !== -1) show = false;
      }

      r.style.display = show ? '' : 'none';
      if (show) {
        visibleRows.push(r);
        if (!firstVisible) firstVisible = r;
      }
    }

    rows.sort(function (a, b) {
      var fa = a.getAttribute('data-featured') === '1' ? 1 : 0;
      var fb = b.getAttribute('data-featured') === '1' ? 1 : 0;
      if (fa !== fb) return fb - fa;
      var dir = state.sortDir;
      if (state.sortBy === 'name') {
        var na = (a.getAttribute('data-name') || '').toLowerCase();
        var nb = (b.getAttribute('data-name') || '').toLowerCase();
        return na.localeCompare(nb) * dir;
      }
      if (state.sortBy === 'boosts') {
        var ba = parseInt(a.getAttribute('data-upvotes') || '0', 10) + parseInt(a.getAttribute('data-burst') || '0', 10);
        var bb = parseInt(b.getAttribute('data-upvotes') || '0', 10) + parseInt(b.getAttribute('data-burst') || '0', 10);
        if (ba !== bb) return (ba - bb) * dir;
      }
      var pa = parseInt(a.getAttribute('data-clients') || '0', 10);
      var pb = parseInt(b.getAttribute('data-clients') || '0', 10);
      if (pa !== pb) return (pa - pb) * dir;
      var ua = parseInt(a.getAttribute('data-upvotes') || '0', 10);
      var ub = parseInt(b.getAttribute('data-upvotes') || '0', 10);
      if (ua !== ub) return ub - ua;
      return (a.getAttribute('data-name') || '').localeCompare(b.getAttribute('data-name') || '');
    });

    for (var s = 0; s < rows.length; s++) {
      listBody.appendChild(rows[s]);
    }

    var visCounter = document.getElementById('nsm-visible-count');
    if (visCounter) visCounter.textContent = String(visibleRows.length);

    var emptyFilterEl = document.getElementById('nsm-empty-filtered');
    if (rows.length > 0 && visibleRows.length === 0) {
      if (!emptyFilterEl) {
        emptyFilterEl = document.createElement('div');
        emptyFilterEl.id = 'nsm-empty-filtered';
        emptyFilterEl.className = 'nsm-empty-list';
        emptyFilterEl.innerHTML = '<h3>No Servers Match Your Active Filters</h3><p>Try clearing your search query or switching tabs above.</p>';
        listBody.appendChild(emptyFilterEl);
      }
      emptyFilterEl.style.display = '';
    } else if (emptyFilterEl) {
      emptyFilterEl.style.display = 'none';
    }

    if (!state.selectedEp && firstVisible) {
      updateInspectorFromRow(firstVisible);
    }
  }

  // ---- Wire up DOM events (callable on initial load and after every SPA navigation) ----
  function initNSMPage() {
    applyAllColorize();
    syncFavoriteButtons();
    updateTabCounters();
    buildFacetsFromDOM();
    filterAndSortRows();

    // Support URL hash for #favorites or #history
    if (window.location.hash === '#favorites' || window.location.hash === '#history') {
      var hashTab = window.location.hash.slice(1);
      state.listType = hashTab;
      var tBtns = document.querySelectorAll('[data-tab]');
      for (var h = 0; h < tBtns.length; h++) {
        tBtns[h].classList.toggle('active', tBtns[h].getAttribute('data-tab') === hashTab);
      }
      filterAndSortRows();
    }

    // Search Input
    var searchInp = document.getElementById('nsm-server-search');
    var clearBtn = document.getElementById('nsm-search-clear');
    if (searchInp && !searchInp.dataset.nsmBound) {
      searchInp.dataset.nsmBound = '1';
      searchInp.addEventListener('input', function () {
        state.searchText = searchInp.value || '';
        if (clearBtn) clearBtn.style.display = state.searchText ? 'inline-block' : 'none';
        filterAndSortRows();
      });
      searchInp.addEventListener('keydown', function (ev) {
        if (ev.key === 'Enter' && /^[a-z0-9.\-]+:\d{2,5}$/i.test(state.searchText.trim())) {
          connectToServer(state.searchText.trim());
        }
      });
    }
    var dcBtn = document.getElementById('nsm-dc-btn');
    if (dcBtn && !dcBtn.dataset.nsmBound) {
      dcBtn.dataset.nsmBound = '1';
      dcBtn.addEventListener('click', function () {
        if (state.searchText.trim()) connectToServer(state.searchText.trim());
      });
    }
    if (clearBtn && searchInp && !clearBtn.dataset.nsmBound) {
      clearBtn.dataset.nsmBound = '1';
      clearBtn.addEventListener('click', function () {
        searchInp.value = '';
        state.searchText = '';
        clearBtn.style.display = 'none';
        filterAndSortRows();
      });
    }

    // Hide Empty / Hide Full
    var hideEmptyEl = document.getElementById('nsm-hide-empty');
    var hideFullEl = document.getElementById('nsm-hide-full');
    if (hideEmptyEl && !hideEmptyEl.dataset.nsmBound) {
      hideEmptyEl.dataset.nsmBound = '1';
      hideEmptyEl.addEventListener('change', function () {
        state.hideEmpty = !!hideEmptyEl.checked;
        updateFilterBadge();
        filterAndSortRows();
      });
    }
    if (hideFullEl && !hideFullEl.dataset.nsmBound) {
      hideFullEl.dataset.nsmBound = '1';
      hideFullEl.addEventListener('change', function () {
        state.hideFull = !!hideFullEl.checked;
        updateFilterBadge();
        filterAndSortRows();
      });
    }

    // Facet search input
    var facetSearchEl = document.getElementById('nsm-facet-search');
    if (facetSearchEl && !facetSearchEl.dataset.nsmBound) {
      facetSearchEl.dataset.nsmBound = '1';
      facetSearchEl.addEventListener('input', function () {
        state.facetSearch = facetSearchEl.value || '';
        buildFacetsFromDOM();
      });
    }

    // Filter & Sort Popovers
    var filterBtn = document.getElementById('nsm-filter-btn');
    var filterPop = document.getElementById('nsm-filter-popover');
    var sortBtn = document.getElementById('nsm-sort-btn');
    var sortPop = document.getElementById('nsm-sort-popover');

    if (filterBtn && filterPop && !filterBtn.dataset.nsmBound) {
      filterBtn.dataset.nsmBound = '1';
      filterBtn.addEventListener('click', function (e) {
        e.stopPropagation();
        var open = filterPop.style.display !== 'none';
        filterPop.style.display = open ? 'none' : 'flex';
        if (sortPop) sortPop.style.display = 'none';
      });
    }
    if (sortBtn && sortPop && !sortBtn.dataset.nsmBound) {
      sortBtn.dataset.nsmBound = '1';
      sortBtn.addEventListener('click', function (e) {
        e.stopPropagation();
        var open = sortPop.style.display !== 'none';
        sortPop.style.display = open ? 'none' : 'block';
        if (filterPop) filterPop.style.display = 'none';
      });
    }

    // Inspector buttons
    var inspConn = document.getElementById('nsm-insp-connect');
    if (inspConn && !inspConn.dataset.nsmBound) {
      inspConn.dataset.nsmBound = '1';
      inspConn.addEventListener('click', function () {
        connectToServer(state.selectedEp || '127.0.0.1:30120');
      });
    }
    var inspBoost = document.getElementById('nsm-insp-boost');
    if (inspBoost && !inspBoost.dataset.nsmBound) {
      inspBoost.dataset.nsmBound = '1';
      inspBoost.addEventListener('click', function () {
        if (state.selectedEp) upvoteServerByEp(state.selectedEp);
      });
    }
    var inspFeature = document.getElementById('nsm-insp-feature');
    if (inspFeature && !inspFeature.dataset.nsmBound) {
      inspFeature.dataset.nsmBound = '1';
      inspFeature.addEventListener('click', function () {
        if (state.selectedEp) featureServerByEp(state.selectedEp);
      });
    }
    var inspCopyJoin = document.getElementById('nsm-insp-copy-join');
    if (inspCopyJoin && !inspCopyJoin.dataset.nsmBound) {
      inspCopyJoin.dataset.nsmBound = '1';
      inspCopyJoin.addEventListener('click', function () {
        var ep = state.selectedEp || '127.0.0.1:30120';
        try {
          navigator.clipboard.writeText('https://nsmultiplayer.com/join/' + ep);
          inspCopyJoin.textContent = 'Copied!';
          setTimeout(function () { inspCopyJoin.textContent = 'Copy'; }, 1400);
        } catch (e) {}
      });
    }
    var inspCopy = document.getElementById('nsm-insp-copy');
    if (inspCopy && !inspCopy.dataset.nsmBound) {
      inspCopy.dataset.nsmBound = '1';
      inspCopy.addEventListener('click', function () {
        var ep = state.selectedEp || '127.0.0.1:30120';
        try {
          navigator.clipboard.writeText('connect ' + ep);
          inspCopy.textContent = 'Copied!';
          setTimeout(function () { inspCopy.textContent = 'Copy'; }, 1400);
        } catch (e) {}
      });
    }

    // Settings Modal
    var settingsBtn = document.getElementById('nsm-settings-btn');
    var settingsModal = document.getElementById('nsm-settings-modal');
    var settingsClose = document.getElementById('nsm-settings-close');
    var themeSelect = document.getElementById('nsm-setting-theme');
    var nickInp = document.getElementById('nsm-setting-nick');
    if (settingsBtn && settingsModal && !settingsBtn.dataset.nsmBound) {
      settingsBtn.dataset.nsmBound = '1';
      settingsBtn.addEventListener('click', function () {
        settingsModal.style.display = 'flex';
      });
    }
    if (settingsClose && settingsModal && !settingsClose.dataset.nsmBound) {
      settingsClose.dataset.nsmBound = '1';
      settingsClose.addEventListener('click', function () {
        settingsModal.style.display = 'none';
      });
    }
    if (themeSelect && !themeSelect.dataset.nsmBound) {
      themeSelect.dataset.nsmBound = '1';
      themeSelect.addEventListener('change', function () {
        document.body.classList.toggle('nsm-light-theme', themeSelect.value === 'light');
      });
    }
    if (nickInp && !nickInp.dataset.nsmBound) {
      nickInp.dataset.nsmBound = '1';
      var savedNick = localStorage.getItem('nickOverride');
      if (savedNick) nickInp.value = savedNick;
      nickInp.addEventListener('input', function () {
        localStorage.setItem('nickOverride', nickInp.value);
      });
    }

    // AFX Support Instant Search
    var hcSearch = document.getElementById('afx-hc-search');
    if (hcSearch && !hcSearch.dataset.nsmBound) {
      hcSearch.dataset.nsmBound = '1';
      hcSearch.addEventListener('input', function () {
        var q = hcSearch.value.trim().toLowerCase();
        var arts = document.querySelectorAll('[data-hc-article]');
        for (var i = 0; i < arts.length; i++) {
          var txt = (arts[i].textContent + ' ' + arts[i].getAttribute('data-hc-article')).toLowerCase();
          var match = !q || txt.indexOf(q) !== -1;
          arts[i].style.display = match ? '' : 'none';
          if (q && match) arts[i].classList.add('open');
        }
      });
    }

    // AFX Marketplace Search & Category Filter
    var mpSearch = document.getElementById('afx-mp-search');
    if (mpSearch && !mpSearch.dataset.nsmBound) {
      mpSearch.dataset.nsmBound = '1';
      mpSearch.addEventListener('input', function () {
        var q = mpSearch.value.trim().toLowerCase();
        var cards = document.querySelectorAll('.afx-mp-card');
        var activeBtn = document.querySelector('#afx-mp-cats [data-mp-cat].active');
        var activeCat = activeBtn ? (activeBtn.getAttribute('data-mp-cat') || 'all') : 'all';
        for (var i = 0; i < cards.length; i++) {
          var cat = cards[i].getAttribute('data-mp-cat') || '';
          var title = (cards[i].getAttribute('data-mp-title') || '') + ' ' + cards[i].textContent.toLowerCase();
          var okCat = activeCat === 'all' || cat === activeCat;
          var okQ = !q || title.indexOf(q) !== -1;
          cards[i].style.display = okCat && okQ ? '' : 'none';
        }
      });
    }

    // NSM Docs Instant Search
    var docsSearch = document.getElementById('afx-docs-search');
    if (docsSearch && !docsSearch.dataset.nsmBound) {
      docsSearch.dataset.nsmBound = '1';
      docsSearch.addEventListener('input', function () {
        var q = docsSearch.value.trim().toLowerCase();
        var nRows = document.querySelectorAll('[data-native-row]');
        for (var i = 0; i < nRows.length; i++) {
          var txt = (nRows[i].textContent + ' ' + nRows[i].getAttribute('data-native-row')).toLowerCase();
          nRows[i].style.display = !q || txt.indexOf(q) !== -1 ? '' : 'none';
        }
      });
    }
  }

  // ---- Flicker-Free Instant SPA Navigation (No Full Page Reload on Click/Submit) ----
  var spaInFlight = false;

  function applySPADocument(htmlText, finalUrl, pushState) {
    var parser = new DOMParser();
    var doc = parser.parseFromString(htmlText, 'text/html');
    if (!doc || !doc.body) {
      window.location.href = finalUrl;
      return;
    }
    if (doc.title) {
      document.title = doc.title;
    }
    document.body.className = doc.body.className || '';
    var bodyStyle = doc.body.getAttribute('style');
    if (bodyStyle) {
      document.body.setAttribute('style', bodyStyle);
    } else {
      document.body.removeAttribute('style');
    }

    var prevScrollY = window.scrollY;
    var samePath = false;
    try {
      var uOld = new URL(window.location.href);
      var uNew = new URL(finalUrl, window.location.origin);
      samePath = (uOld.pathname === uNew.pathname);
    } catch (e) {}

    document.body.innerHTML = doc.body.innerHTML;

    // Execute inline page scripts (except main.js which is already active)
    var scripts = document.body.querySelectorAll('script');
    for (var i = 0; i < scripts.length; i++) {
      var oldScript = scripts[i];
      var src = oldScript.getAttribute('src') || '';
      if (src.indexOf('/js/main.js') !== -1) continue;
      var newScript = document.createElement('script');
      for (var a = 0; a < oldScript.attributes.length; a++) {
        var attr = oldScript.attributes[a];
        newScript.setAttribute(attr.name, attr.value);
      }
      newScript.textContent = oldScript.textContent;
      oldScript.parentNode.replaceChild(newScript, oldScript);
    }

    if (pushState) {
      try {
        window.history.pushState({ nsmSpa: true }, document.title, finalUrl);
      } catch (e) {}
    }

    if (samePath) {
      window.scrollTo(0, prevScrollY);
    } else {
      window.scrollTo(0, 0);
    }

    state.selectedEp = '';
    initNSMPage();
  }

  function nsmNavigate(url, pushState) {
    if (spaInFlight) return;
    spaInFlight = true;
    fetch(url, {
      method: 'GET',
      credentials: 'same-origin',
      headers: { 'X-NSM-SPA': '1' }
    })
      .then(function (res) {
        var ct = (res.headers.get('content-type') || '').toLowerCase();
        if (ct.indexOf('text/html') === -1) {
          window.location.href = res.url || url;
          return null;
        }
        return res.text().then(function (txt) {
          return { text: txt, url: res.url || url };
        });
      })
      .then(function (payload) {
        spaInFlight = false;
        if (!payload) return;
        applySPADocument(payload.text, payload.url, pushState !== false);
      })
      .catch(function () {
        spaInFlight = false;
        window.location.href = url;
      });
  }

  window.addEventListener('popstate', function () {
    nsmNavigate(window.location.href, false);
  });

  document.addEventListener('DOMContentLoaded', function () {
    initNSMPage();

    // Intercept form submissions for flicker-free updates
    document.addEventListener('submit', function (e) {
      var form = e.target;
      if (!form || form.tagName !== 'FORM') return;
      if (form.hasAttribute('data-no-spa')) return;
      var action = form.getAttribute('action') || window.location.href;
      var method = (form.getAttribute('method') || 'GET').toUpperCase();
      try {
        var actionUrl = new URL(action, window.location.origin);
        if (actionUrl.origin !== window.location.origin) return;
        if (actionUrl.pathname === '/logout') return;
        e.preventDefault();
        var fd = new FormData(form);
        if (e.submitter && e.submitter.name) {
          fd.set(e.submitter.name, e.submitter.value || '');
        }
        if (method === 'GET') {
          var params = new URLSearchParams(fd);
          var targetGet = actionUrl.pathname + (params.toString() ? '?' + params.toString() : '');
          nsmNavigate(targetGet, true);
          return;
        }
        var enc = (form.getAttribute('enctype') || '').toLowerCase();
        var isMultipart = enc.indexOf('multipart/form-data') !== -1 || !!form.querySelector('input[type="file"]');
        var fetchOpts = {
          method: method,
          credentials: 'same-origin',
          headers: { 'X-NSM-SPA': '1' },
          body: isMultipart ? fd : new URLSearchParams(fd).toString()
        };
        if (!isMultipart) {
          fetchOpts.headers['Content-Type'] = 'application/x-www-form-urlencoded;charset=UTF-8';
        }
        fetch(actionUrl.href, fetchOpts)
          .then(function (res) {
            var ct = (res.headers.get('content-type') || '').toLowerCase();
            if (ct.indexOf('text/html') === -1) {
              window.location.href = res.url || actionUrl.href;
              return null;
            }
            return res.text().then(function (txt) {
              return { text: txt, url: res.url || actionUrl.href };
            });
          })
          .then(function (payload) {
            if (!payload) return;
            applySPADocument(payload.text, payload.url, true);
          })
          .catch(function () {
            form.setAttribute('data-no-spa', '1');
            form.submit();
          });
      } catch (err) {}
    });

    // Global delegated click handler
    document.addEventListener('click', function (e) {
      var target = e.target;
      if (!target) return;

      var filterPop = document.getElementById('nsm-filter-popover');
      var filterBtn = document.getElementById('nsm-filter-btn');
      var sortPop = document.getElementById('nsm-sort-popover');
      var sortBtn = document.getElementById('nsm-sort-btn');
      var facetSearchEl = document.getElementById('nsm-facet-search');
      var hcSearch = document.getElementById('afx-hc-search');

      if (filterPop && !filterPop.contains(target) && (!filterBtn || !filterBtn.contains(target))) {
        filterPop.style.display = 'none';
      }
      if (sortPop && !sortPop.contains(target) && (!sortBtn || !sortBtn.contains(target))) {
        sortPop.style.display = 'none';
      }

      // ListTypeTabs (.mp-tabular-item or .nsm-tab-btn)
      var tabBtn = target.closest('[data-tab]');
      if (tabBtn) {
        var allTabs = document.querySelectorAll('[data-tab]');
        for (var i = 0; i < allTabs.length; i++) allTabs[i].classList.remove('active');
        tabBtn.classList.add('active');
        state.listType = tabBtn.getAttribute('data-tab') || 'all';
        filterAndSortRows();
        return;
      }

      // Facet tab switch
      var fTab = target.closest('.nsm-facet-tab');
      if (fTab) {
        e.stopPropagation();
        var allFT = document.querySelectorAll('.nsm-facet-tab');
        for (var f = 0; f < allFT.length; f++) allFT[f].classList.remove('active');
        fTab.classList.add('active');
        var which = fTab.getAttribute('data-facet-tab');
        var tagsList = document.getElementById('nsm-facet-tags-list');
        var locsList = document.getElementById('nsm-facet-locales-list');
        if (tagsList) tagsList.style.display = which === 'tags' ? '' : 'none';
        if (locsList) locsList.style.display = which === 'locales' ? '' : 'none';
        if (facetSearchEl) facetSearchEl.placeholder = which === 'tags' ? 'Search tags...' : 'Search locales...';
        return;
      }

      // Faucet click (cycle 0 -> 1 -> -1 -> 0)
      var faucet = target.closest('.nsm-faucet');
      if (faucet) {
        e.stopPropagation();
        var kind = faucet.getAttribute('data-faucet-kind');
        var key = faucet.getAttribute('data-faucet-key');
        var map = kind === 'locale' ? state.localeFacets : state.tagFacets;
        var cur = map[key] || 0;
        var next = cur === 0 ? 1 : (cur === 1 ? -1 : 0);
        if (next === 0) delete map[key];
        else map[key] = next;
        buildFacetsFromDOM();
        filterAndSortRows();
        return;
      }

      // Remove search chip
      var remTag = target.closest('[data-remove-tag]');
      if (remTag) {
        delete state.tagFacets[remTag.getAttribute('data-remove-tag')];
        buildFacetsFromDOM();
        filterAndSortRows();
        return;
      }
      var remLoc = target.closest('[data-remove-locale]');
      if (remLoc) {
        delete state.localeFacets[remLoc.getAttribute('data-remove-locale')];
        buildFacetsFromDOM();
        filterAndSortRows();
        return;
      }

      // Click tag loaf inside row
      var tagLoaf = target.closest('[data-tag-click]');
      if (tagLoaf) {
        e.stopPropagation();
        var tName = tagLoaf.getAttribute('data-tag-click');
        state.tagFacets[tName] = state.tagFacets[tName] === 1 ? 0 : 1;
        if (!state.tagFacets[tName]) delete state.tagFacets[tName];
        buildFacetsFromDOM();
        filterAndSortRows();
        return;
      }

      // Sort option click
      var sortItem = target.closest('.nsm-sort-item');
      if (sortItem) {
        e.stopPropagation();
        var sb = sortItem.getAttribute('data-sort');
        if (state.sortBy === sb) {
          state.sortDir = state.sortDir === -1 ? 1 : -1;
        } else {
          state.sortBy = sb;
          state.sortDir = sb === 'name' ? 1 : -1;
        }
        var allSorts = document.querySelectorAll('.nsm-sort-item');
        for (var s = 0; s < allSorts.length; s++) {
          var isCur = allSorts[s].getAttribute('data-sort') === state.sortBy;
          allSorts[s].classList.toggle('active', isCur);
          var ind = allSorts[s].querySelector('[data-sort-ind]');
          if (ind) ind.innerHTML = isCur ? (state.sortDir === -1 ? '&#8595;' : '&#8593;') : '';
        }
        var lblMap = { players: 'Players', boosts: 'Boosts', name: 'Name' };
        var sortLbl = document.getElementById('nsm-sort-label');
        var sortIcon = document.getElementById('nsm-sort-dir-icon');
        if (sortLbl) sortLbl.textContent = lblMap[state.sortBy] || 'Players';
        if (sortIcon) sortIcon.innerHTML = state.sortDir === -1 ? '&#8595;' : '&#8593;';
        filterAndSortRows();
        return;
      }

      // Connect button
      var connBtn = target.closest('[data-connect-ep]');
      if (connBtn) {
        e.stopPropagation();
        connectToServer(connBtn.getAttribute('data-connect-ep'));
        return;
      }

      // Upvote button
      var upvBtn = target.closest('[data-upvote-ep]');
      if (upvBtn) {
        e.stopPropagation();
        upvoteServerByEp(upvBtn.getAttribute('data-upvote-ep'));
        return;
      }

      // Owner Feature button (nta~ only)
      var featBtn = target.closest('[data-feature-ep]');
      if (featBtn) {
        e.stopPropagation();
        featureServerByEp(featBtn.getAttribute('data-feature-ep'));
        return;
      }

      // Favorite star
      var favBtn = target.closest('[data-fav-ep]');
      if (favBtn) {
        e.stopPropagation();
        toggleFavorite(favBtn.getAttribute('data-fav-ep'));
        return;
      }

      // Featured Server card click -> select in Inspector
      var featCard = target.closest('.nsm-featured-card');
      if (featCard && !target.closest('a') && !target.closest('button')) {
        var fEp = featCard.getAttribute('data-featured-card');
        var fRow = fEp ? document.querySelector('.nsm-server-row[data-ep="' + fEp + '"]') : null;
        if (fRow) updateInspectorFromRow(fRow);
        return;
      }

      // Server Row click -> select in Inspector (or navigate if no inspector on page)
      var srvRow = target.closest('.nsm-server-row');
      if (srvRow && !target.closest('a') && !target.closest('button')) {
        var insp = document.getElementById('nsm-inspector-card');
        if (insp) {
          updateInspectorFromRow(srvRow);
        } else {
          var ep = srvRow.getAttribute('data-ep');
          if (ep) window.location.href = '/servers/' + encodeURIComponent(ep);
        }
        return;
      }

      // AFX Support quick filter tag
      var hcFilter = target.closest('[data-hc-filter]');
      if (hcFilter && hcSearch) {
        hcSearch.value = hcFilter.getAttribute('data-hc-filter') || '';
        hcSearch.dispatchEvent(new Event('input'));
        return;
      }

      // AFX Marketplace category tab
      var mpCatBtn = target.closest('[data-mp-cat]');
      if (mpCatBtn && mpCatBtn.closest('#afx-mp-cats')) {
        var allMp = document.querySelectorAll('#afx-mp-cats [data-mp-cat]');
        for (var m = 0; m < allMp.length; m++) allMp[m].classList.remove('active');
        mpCatBtn.classList.add('active');
        var mpInp = document.getElementById('afx-mp-search');
        if (mpInp) mpInp.dispatchEvent(new Event('input'));
        return;
      }

      // AFX Marketplace Get Resource modal
      var mpInst = target.closest('[data-mp-install]');
      if (mpInst) {
        var resId = mpInst.getAttribute('data-mp-install');
        var resName = mpInst.getAttribute('data-mp-name') || resId;
        var modal = document.getElementById('afx-mp-modal');
        var mTitle = document.getElementById('afx-mp-modal-title');
        var mCode = document.getElementById('afx-mp-modal-code');
        if (mTitle) mTitle.textContent = 'Install ' + resName;
        if (mCode) mCode.textContent = 'ensure ' + resId;
        if (modal) modal.style.display = 'flex';
        return;
      }
      if (target.id === 'afx-mp-modal-close' || target.id === 'afx-mp-modal') {
        var mEl = document.getElementById('afx-mp-modal');
        if (mEl) mEl.style.display = 'none';
        return;
      }
      if (target.id === 'afx-mp-modal-copy') {
        var cEl = document.getElementById('afx-mp-modal-code');
        if (cEl && navigator.clipboard) {
          navigator.clipboard.writeText(cEl.textContent || '');
          target.textContent = 'Copied!';
          setTimeout(function () { target.textContent = 'Copy'; }, 1400);
        }
        return;
      }

      // FAQ Accordion (.nsm-faq-q)
      var faqBtn = target.closest('.nsm-faq-q');
      if (faqBtn) {
        var item = faqBtn.closest('.nsm-faq-item');
        if (item) item.classList.toggle('open');
        return;
      }

      // Flicker-free SPA navigation for internal links
      if (!e.defaultPrevented && e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey) {
        var anchor = target.closest('a[href]');
        if (anchor && !anchor.hasAttribute('download') && !anchor.hasAttribute('data-no-spa')) {
          var rawHref = anchor.getAttribute('href') || '';
          if (rawHref && rawHref.charAt(0) !== '#' && rawHref.indexOf('javascript:') !== 0 && rawHref.indexOf('mailto:') !== 0 && rawHref.indexOf('fivem://') !== 0) {
            try {
              var linkUrl = new URL(rawHref, window.location.origin);
              if (linkUrl.origin === window.location.origin && linkUrl.pathname !== '/logout' && linkUrl.pathname !== '/download' && linkUrl.pathname !== '/NSM.exe' && linkUrl.pathname !== '/download-server' && linkUrl.pathname !== '/NSM-Server.zip' && linkUrl.pathname.indexOf('/api/') !== 0) {
                e.preventDefault();
                nsmNavigate(linkUrl.href, true);
                return;
              }
            } catch (err) {}
          }
        }
      }
    });

    // Live polling every 8s to keep player/server/upvote counts fresh
    setInterval(function () {
      fetch('/api/servers/live', { headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (data) {
          if (!data) return;
          function nsmFmtK(n) { n = Number(n) || 0; if (n >= 1000000) return (n / 1000000).toFixed(1).replace(/\.0$/, '') + 'm'; if (n >= 1000) return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k'; return String(n); }
          var pEls = document.querySelectorAll('[data-stat="players"]');
          for (var i = 0; i < pEls.length; i++) pEls[i].textContent = nsmFmtK(data.players || 0);
          var sEls = document.querySelectorAll('[data-stat="servers"]');
          for (var j = 0; j < sEls.length; j++) sEls[j].textContent = String(data.count || 0);
          if (Array.isArray(data.servers)) {
            for (var k = 0; k < data.servers.length; k++) {
              var srv = data.servers[k];
              var ep = srv.EndPoint || srv.id;
              if (!ep) continue;
              var upv = typeof srv.Upvotes === 'number' ? srv.Upvotes : (srv.upvotes || 0);
              var upNodes = document.querySelectorAll('[data-upvote-count="' + ep + '"]');
              for (var u = 0; u < upNodes.length; u++) {
                upNodes[u].textContent = String(upv);
              }
              var row = document.querySelector('.nsm-server-row[data-ep="' + ep + '"]');
              if (row) {
                row.setAttribute('data-upvotes', String(upv));
                row.setAttribute('data-clients', String(srv.Clients || srv.players || 0));
                var curEl = row.querySelector('.nsm-srv-players .cur');
                if (curEl) curEl.textContent = String(srv.Clients || srv.players || 0);
              }
              var inspUp = document.getElementById('nsm-insp-upvotes');
              var inspEp = document.getElementById('nsm-insp-ep');
              if (inspUp && inspEp && inspEp.textContent === ep) {
                inspUp.textContent = String(upv);
              }
            }
          }
        })
        .catch(function () {});
    }, 8000);
  });
})();
