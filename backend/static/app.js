// API base URL: injected by Docker entrypoint from API_URL env.
// Empty or "/api" = same-domain (nginx proxies /api to backend); full URL = separate backend (we append /api).
function getApiBase() {
  const apiUrl = (typeof window.APP_CONFIG !== 'undefined' && window.APP_CONFIG.apiUrl !== undefined) ? window.APP_CONFIG.apiUrl : "";
  if (apiUrl === "" || apiUrl === null || apiUrl === undefined) return "/api";
  return apiUrl.replace(/\/$/, "") + "/api";
}
const apiBase = getApiBase();

// Log level: DEBUG logs only when LOG_LEVEL=DEBUG (injected via APP_CONFIG.logLevel or fetched from /api/config)
const logLevel = (typeof window.APP_CONFIG !== 'undefined' && window.APP_CONFIG.logLevel)
  ? window.APP_CONFIG.logLevel.toUpperCase()
  : "INFO";

function debugLog(...args) {
  if (logLevel === "DEBUG") {
    console.log(...args);
  }
}

function showToast(message, type = "error", durationMs = 4500) {
  if (!message) return;
  let container = document.getElementById("toast-container");
  if (!container) {
    container = document.createElement("div");
    container.id = "toast-container";
    document.body.appendChild(container);
  }

  const toast = document.createElement("div");
  toast.className = `toast ${type === "info" ? "toast-info" : ""}`.trim();
  toast.textContent = message;
  container.appendChild(toast);

  requestAnimationFrame(() => toast.classList.add("toast-show"));
  setTimeout(() => {
    toast.classList.remove("toast-show");
    setTimeout(() => {
      toast.remove();
    }, 200);
  }, durationMs);
}

const Config = {
  endpoints: {
    systemsSearch: `${apiBase}/systems/search`,
    nearTradeHubs: `${apiBase}/near_trade_hubs.html`,
    proximity: `${apiBase}/proximity.html`,

    authLocation: `${apiBase}/auth/location`,
    authLogin: `${apiBase}/auth/login`,
    authLogout: `${apiBase}/auth/logout`,
    authWaypoint: `${apiBase}/auth/waypoint`,
  }
};

// Per-mode cache of table HTML; restored when switching back to a mode
const modeResultsCache = {
  near_trade_hubs: null,
  proximity: null
};

// True after first handleModeChange() from initial load (so we don't fetch near_trade_hubs on load)
let initialLoadDone = false;

// Store current location to restore when switching back to proximity mode
let currentLocationData = null; // { systemID, systemName }

// Suppress CSS [data-tooltip] tooltips in a container (mobile sticky hover fix).
// Temporarily removes the data-tooltip attribute so the :hover pseudo-element disappears.
function hideTooltipsIn(container) {
  if (!container) return;
  container.querySelectorAll('[data-tooltip]').forEach(el => {
    const saved = el.getAttribute('data-tooltip');
    el.removeAttribute('data-tooltip');
    setTimeout(() => {
      if (saved) el.setAttribute('data-tooltip', saved);
    }, 150);
  });
}

// Shared logic for attackers-toggle (show)/(hide) clicks.
// Called by both the early pre-DOMContentLoaded delegation and the later bindEventHandlers delegation.
function handleAttackersToggle(toggle) {
  var targetId = toggle.getAttribute("data-target");
  var target = targetId && document.getElementById(targetId);
  if (!target) return;
  var expanding = !target.classList.contains("expanded");
  target.classList.toggle("expanded", expanding);
  toggle.textContent = expanding ? "(hide)" : "(show)";
}

// DOM Ready Handler
document.addEventListener('DOMContentLoaded', () => {
  debugLog("CLIENT: DOM ready, initializing application");
  // Check if we just came back from authentication
  const urlParams = new URLSearchParams(window.location.search);
  const isAuthRedirect = urlParams.get('auth') === 'success';
  if (isAuthRedirect) {
    // Remove the query parameter from URL
    window.history.replaceState({}, document.title, window.location.pathname);
    debugLog("CLIENT: Detected successful authentication redirect");
  }

  handlePageReload();
  initializeApplication().catch(error => {
    console.error("CLIENT: Initialization failed:", error);
  });
});

function handlePageReload() {
  // Check if there's a saved mode in localStorage
  const savedMode = localStorage.getItem('evePvpSearchMode');
  if (savedMode) {
    const modeInput = document.querySelector(`input[name='mode'][value='${savedMode}']`);
    if (modeInput) {
      modeInput.checked = true;
    }
  }
  
  // Restore checkbox and filter state
  restoreFilterState();
  
  // Check if we just came back from authentication
  const urlParams = new URLSearchParams(window.location.search);
    if (urlParams.get('auth') === 'success') {
    // Remove the query parameter from URL
    window.history.replaceState({}, document.title, window.location.pathname);
  }
  // Mode display and load are handled once in initializeApplication() to avoid duplicate popups
}

async function initializeApplication() {
  try {
    ensureCcpFooterExists(); // Footer is sibling of result-container so visible even with no results or unauthenticated
    await loadInitialData();
    bindEventHandlers();
    // Apply auth state from server-embedded data (no network request)
    const isAuthenticated = applyAuthFromPage();
    // Fetch character militia from ESI if not already embedded in auth data
    if (isAuthenticated) {
      tryFetchMilitiaFromServer();
    }
    // Initialize mode selection (for proximity mode this may show login popup or load table)
    await handleModeChange();
    restoreFilterState();
    initialLoadDone = true; // Allow fetch when user later switches to near_trade_hubs from another mode
    // Ensure buttons are shown/hidden correctly after mode change (in case cached data was restored)
    // Use requestAnimationFrame to ensure DOM updates are complete
    requestAnimationFrame(() => {
      if (isAuthenticated) {
        showSetDestinationButtons();
      } else {
        hideSetDestinationButtons();
      }
    });
    // Table load for trade-hubs and proximity is already done in handleModeChange()
  } catch (error) {
    console.error("Initialization failed:", error);
  }
}

// Apply auth state from server-embedded data (no network request)
function applyAuthFromPage() {
  const data = window.__auth || { authenticated: false };
  const loggedOutDiv = document.getElementById("auth-logged-out");
  const loggedInDiv = document.getElementById("auth-logged-in");
  const characterNameSpan = document.getElementById("character-name");
  const characterPortrait = document.getElementById("character-portrait");

  if (data.authenticated) {
    if (loggedOutDiv) loggedOutDiv.style.display = "none";
    if (loggedInDiv) loggedInDiv.style.display = "flex";
    const characterID = data.characterID ?? data.character_id;
    if (characterNameSpan) {
      const name = data.characterName || data.character_name || (characterID ? `Character ${characterID}` : "Character");
      characterNameSpan.textContent = name;
    }
    if (characterPortrait && characterID) {
      const portraitUrl = `https://images.evetech.net/characters/${characterID}/portrait?size=64`;
      if (characterPortrait.src !== portraitUrl) {
        characterPortrait.src = portraitUrl;
      }
      characterPortrait.alt = characterNameSpan ? characterNameSpan.textContent : "Character portrait";
      characterPortrait.style.display = "block";
      characterPortrait.onerror = function() { this.style.display = "none"; };
    }
    // Show faction icon if character has a militia
    const factionIcon = document.getElementById("militia-faction-icon");
    const clockIcon = document.getElementById("militia-clock-icon");
    const militiaShortName = data.militiaShortName || "";
    const showMilitia = !!militiaShortName;
    if (factionIcon) {
      factionIcon.style.display = showMilitia ? "inline-block" : "none";
      if (militiaShortName) {
        factionIcon.className = "militia-icon militia-icon--" + militiaShortName;
      }
    }
    // Clock tooltip — visible when logged in unless user dismissed it
    if (clockIcon) {
      clockIcon.style.display = localStorage.getItem("militia_clock_dismissed") === "1" ? "none" : "inline";
      clockIcon.onclick = function() {
        this.style.display = "none";
        localStorage.setItem("militia_clock_dismissed", "1");
      };
    }
    showSetDestinationButtons();
  } else {
    if (loggedOutDiv) loggedOutDiv.style.display = "block";
    if (loggedInDiv) loggedInDiv.style.display = "none";
    if (characterPortrait) {
      characterPortrait.src = "";
      characterPortrait.alt = "";
      characterPortrait.style.display = "none";
    }
    hideSetDestinationButtons();
  }
  applyMilitiaFilter();
  return !!data.authenticated;
}

async function loadInitialData() {
  // No initial data loading needed
}

// Fetch character militia shortName from server and update UI.
// Retries until we get a definite answer ("" = not enlisted, or a faction shortName)
// or time out (handles the race where the SSO callback's ESI fetch hasn't
// completed yet when the page first renders).
async function tryFetchMilitiaFromServer() {
  for (let attempt = 0; attempt < 10; attempt++) {
    try {
      const resp = await fetch("/api/auth/militia");
      if (!resp.ok) return;
      const data = await resp.json();
      if (data.militiaShortName !== undefined) {
        if (window.__auth) {
          window.__auth.militiaShortName = data.militiaShortName || "";
        }
        applyAuthFromPage();
        return;
      }
    } catch (e) { /* retry */ }
    await new Promise(r => setTimeout(r, 500));
  }
}


function bindEventHandlers() {
  debugLog("CLIENT: Binding event handlers");
  const modeInputs = document.querySelectorAll("input[name='mode']");
  modeInputs.forEach(input => {
    input.addEventListener("change", handleModeChange);
  });

  // Thera camps one-time setup (toggle handled by early delegation)
  const theraCampsContainer = document.getElementById("thera-camps-container");
  if (theraCampsContainer) {
    ensureShipTypeIconsUseWikiLinks(theraCampsContainer);
    bindTheraFloatingTooltips(theraCampsContainer);
  }

  // Jump clone table is always server-rendered; only bind buttons and wire up checkboxes (no client fetch)
  bindJumpCloneDestinationButtons();
  document.querySelectorAll(".trade-hub-checkbox").forEach(cb => {
    cb.addEventListener("change", function() {
      const term = (this.getAttribute("data-trade-hub") || "").toLowerCase();
      if (!term) return;
      if (this.checked) {
        hiddenTradeHubs.delete(term);
      } else {
        hiddenTradeHubs.add(term);
      }
      applySecurityFilters();
    });
  });


  // Handle distance cell clicks to show/hide routes (using event delegation)
  document.addEventListener("click", function(e) {
    // Check if the clicked element or its parent is the distance-value
    let distanceValue = null;
    if (e.target && e.target.classList.contains("distance-value")) {
      distanceValue = e.target;
    } else if (e.target && e.target.closest(".distance-value")) {
      distanceValue = e.target.closest(".distance-value");
    }
    
    if (distanceValue) {
      const cell = distanceValue.closest("td");
      if (cell) {
        const routeContainer = cell.querySelector(".route-container");
        if (routeContainer) {
          routeContainer.classList.toggle("expanded");
        }
        hideTooltipsIn(cell);
      }
    }

    const pilotFilterBtn = e.target && e.target.closest && e.target.closest(".pilot-hide-systems-btn");
    if (pilotFilterBtn) {
      e.preventDefault();
      e.stopPropagation();
      const id = pilotFilterBtn.getAttribute("data-character-id");
      if (!id) return;
      if (pilotOnlyCharacterIds.has(id)) {
        pilotOnlyCharacterIds.delete(id);
      } else {
        pilotOnlyCharacterIds.add(id);
      }
      document.querySelectorAll(`.pilot-hide-systems-btn[data-character-id="${id}"]`).forEach(btn => {
        const on = pilotOnlyCharacterIds.has(id);
        btn.setAttribute("aria-pressed", on ? "true" : "false");
        btn.classList.toggle("active", on);
        btn.setAttribute("title", on ? "Clear filter (show all systems again)" : "Show only systems where this pilot appears");
        btn.setAttribute("aria-label", on ? "Pilot filter active. Click to show all systems again." : "Show only systems where this pilot appears as an attacker");
      });
      applySecurityFilters();
      return;
    }

    const tradeHubFilterBtn = e.target && e.target.closest && e.target.closest(".trade-hub-filter-btn");
    if (tradeHubFilterBtn) {
      e.preventDefault();
      e.stopPropagation();
      const term = (tradeHubFilterBtn.getAttribute("data-trade-hub-filter") || "").trim().toLowerCase();
      if (!term) return;

      // Collect all trade hub names from checkboxes (preferred) or filter buttons (fallback).
      const allHubs = new Set();
      document.querySelectorAll(".trade-hub-checkbox").forEach(cb => {
        const t = cb.getAttribute("data-trade-hub");
        if (t) allHubs.add(t);
      });
      if (allHubs.size === 0) {
        document.querySelectorAll(".trade-hub-filter-btn").forEach(btn => {
          const t = (btn.getAttribute("data-trade-hub-filter") || "").trim().toLowerCase();
          if (t) allHubs.add(t);
        });
      }

      // Single-select: if every hub except this one is already hidden → show all.
      // Otherwise → show only this hub.
      let onlyThisShown = allHubs.size > 1;
      for (const h of allHubs) {
        if (h !== term && !hiddenTradeHubs.has(h)) {
          onlyThisShown = false;
          break;
        }
      }

      if (onlyThisShown) {
        hiddenTradeHubs.clear();
      } else {
        for (const h of allHubs) {
          if (h !== term) {
            hiddenTradeHubs.add(h);
          } else {
            hiddenTradeHubs.delete(h);
          }
        }
      }

      syncTradeHubCheckboxes();
      applySecurityFilters();
      return;
    }

    // Handle attackers list toggle using event delegation (works with dynamically updated content)
    const attackersToggle = e.target && e.target.closest && e.target.closest(".attackers-toggle");
    if (attackersToggle) {
      e.preventDefault();
      e.stopPropagation();
      handleAttackersToggle(attackersToggle);
      var toggleTargetId = attackersToggle.getAttribute("data-target");
      var toggleTarget = toggleTargetId && document.getElementById(toggleTargetId);
      if (toggleTarget) hideTooltipsIn(toggleTarget);
      return;
    }
    
    // Handle anchor links to system rows - prevent default scroll and scroll manually
    if (e.target && e.target.tagName === 'A' && e.target.getAttribute('href') && e.target.getAttribute('href').startsWith('#system-')) {
      e.preventDefault();
      const targetId = e.target.getAttribute('href').substring(1); // Remove #
      const targetElement = document.getElementById(targetId);
      if (targetElement) {
        // Scroll to element smoothly without changing URL hash
        targetElement.scrollIntoView({ behavior: 'smooth', block: 'center' });
      }
    }

    // Hide tooltip on any [data-tooltip] element click (mobile sticky hover fix)
    if (e.target && e.target.hasAttribute && e.target.hasAttribute('data-tooltip')) {
      hideTooltipsIn(e.target);
    }
  });

  // Keyboard accessibility for pilot filter toggles rendered as icon spans.
  document.addEventListener("keydown", function(e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    const toggle = e.target && e.target.closest && e.target.closest(".pilot-hide-systems-btn");
    if (!toggle) return;
    e.preventDefault();
    toggle.click();
  });

  // Bind security filter checkboxes
  const filterLowsec = document.getElementById("filter-lowsec");
  const filterNullsec = document.getElementById("filter-nullsec");
  const filterGatecamps = document.getElementById("filter-gatecamps");
  
  if (filterLowsec) {
    filterLowsec.addEventListener("change", applySecurityFilters);
  }
  if (filterNullsec) {
    filterNullsec.addEventListener("change", applySecurityFilters);
  }
  if (filterGatecamps) {
    filterGatecamps.addEventListener("change", applySecurityFilters);
  }
  const maxAttackersInc = document.getElementById("max-attackers-inc");
  const maxAttackersDec = document.getElementById("max-attackers-dec");
  const maxAttackersReset = document.getElementById("max-attackers-reset");
  if (maxAttackersInc) {
    maxAttackersInc.addEventListener("click", function() {
      if (maxAttackersLimit < 1) {
        setMaxAttackersLimit(1);
      } else {
        setMaxAttackersLimit(Math.min(maxAttackersLimit + 1, 100));
      }
    });
  }
  if (maxAttackersDec) {
    maxAttackersDec.addEventListener("click", function() {
      if (maxAttackersLimit < 1) {
        setMaxAttackersLimit(1);
      } else {
        setMaxAttackersLimit(Math.max(maxAttackersLimit - 1, 1));
      }
    });
  }
  if (maxAttackersReset) {
    maxAttackersReset.addEventListener("click", function() {
      setMaxAttackersLimit(-1);
    });
  }

  // Highsec checkbox persistence and handler
  const militiaHighsec = document.getElementById("militia-highsec");
  if (militiaHighsec) {
    const saved = localStorage.getItem("evePvpSearchMilitiaHighsec");
    militiaHighsec.checked = saved !== "false";
    militiaHighsec.addEventListener("change", function() {
      localStorage.setItem("evePvpSearchMilitiaHighsec", this.checked ? "true" : "false");
      applySecurityFilters();
    });
  }

  debugLog("CLIENT: Event handlers bound");
}

function bindTheraFloatingTooltips(theraCampsContainer) {
  if (!theraCampsContainer) return;

  // Avoid binding twice on hot reload / repeated initializeApplication calls.
  if (theraCampsContainer.dataset.floatingTooltipsBound === "true") return;
  theraCampsContainer.dataset.floatingTooltipsBound = "true";

  let tooltipEl = document.getElementById("thera-floating-tooltip");
  if (!tooltipEl) {
    tooltipEl = document.createElement("div");
    tooltipEl.id = "thera-floating-tooltip";
    tooltipEl.setAttribute("role", "tooltip");
    tooltipEl.setAttribute("aria-hidden", "true");
    document.body.appendChild(tooltipEl);
  }

  const tooltipTargets = theraCampsContainer.querySelectorAll(".pilot-link[data-tooltip], .ship-type-icon[data-tooltip]");
  if (!tooltipTargets || tooltipTargets.length === 0) return;

  const scroller = theraCampsContainer.querySelector(".thera-camps-content");
  const margin = 6;        // vertical gap between icon and tooltip
  const sidePadding = 8;  // viewport left/right padding for clamping
  const minTopGap = 4;

  let activeTarget = null;
  let hideTimer = null;

  function clamp(val, min, max) {
    return Math.max(min, Math.min(max, val));
  }

  function positionTooltipFor(target) {
    if (!target) return;

    const text = (target.getAttribute("data-tooltip") || "").trim();
    if (!text) return;

    tooltipEl.textContent = text;
    tooltipEl.style.maxWidth = "none";
    tooltipEl.classList.add("is-visible");

    // Measure after it becomes visible.
    const tipRect = tooltipEl.getBoundingClientRect();
    const targetRect = target.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;

    const preferAbove = targetRect.top >= tipRect.height + margin + minTopGap;
    const topAbove = targetRect.top - tipRect.height - margin;
    const topBelow = targetRect.bottom + margin;

    const top = preferAbove ? topAbove : topBelow;

    // Center horizontally relative to the icon, then clamp to viewport.
    const desiredLeft = targetRect.left + (targetRect.width / 2) - (tipRect.width / 2);
    const left = clamp(desiredLeft, sidePadding, vw - tipRect.width - sidePadding);

    // If both above and below go off-screen, clamp top anyway.
    const clampedTop = clamp(top, sidePadding, vh - tipRect.height - sidePadding);

    tooltipEl.style.left = `${left}px`;
    tooltipEl.style.top = `${clampedTop}px`;
    tooltipEl.style.position = "fixed";
  }

  function show(target) {
    if (hideTimer) {
      clearTimeout(hideTimer);
      hideTimer = null;
    }
    activeTarget = target;
    tooltipEl.setAttribute("aria-hidden", "false");
    positionTooltipFor(target);
  }

  function scheduleHide() {
    if (hideTimer) clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      activeTarget = null;
      tooltipEl.classList.remove("is-visible");
      tooltipEl.setAttribute("aria-hidden", "true");
    }, 60); // small delay to avoid flicker when moving between elements
  }

  function hide() {
    if (hideTimer) clearTimeout(hideTimer);
    activeTarget = null;
    tooltipEl.classList.remove("is-visible");
    tooltipEl.setAttribute("aria-hidden", "true");
  }

  tooltipTargets.forEach(target => {
    target.addEventListener("mouseenter", () => show(target));
    target.addEventListener("mouseleave", () => {
      if (activeTarget === target) scheduleHide();
    });
    target.addEventListener("click", () => {
      if (activeTarget === target) hide();
    });
  });

  // Keep tooltip aligned while scrolling the Thera block.
  if (scroller) {
    scroller.addEventListener("scroll", () => {
      if (!activeTarget) return;
      positionTooltipFor(activeTarget);
    });
  }

  window.addEventListener("resize", () => {
    if (!activeTarget) return;
    positionTooltipFor(activeTarget);
  });
}

// Fetch and display current location when authenticated
async function fetchAndSetCurrentLocation() {
  try {
    debugLog("CLIENT: Fetching current location from:", Config.endpoints.authLocation);
    const response = await fetch(Config.endpoints.authLocation, {
      credentials: 'include' // Include cookies in cross-origin requests
    });
    if (!response.ok) {
      if (response.status === 401) {
        // Not authenticated or token expired
        debugLog("CLIENT: Not authenticated, cannot fetch location");
        hideLocationDisplay();
        return null;
      }
      const errorText = await response.text();
      console.error("CLIENT: Failed to fetch location:", response.status, errorText);
      throw new Error("Failed to fetch location");
    }
    const data = await response.json();
    debugLog("CLIENT: Location data received:", data);
    if (data.systemID && data.systemName) {
      // Display the location
      showLocationDisplay(data.systemName, data.systemID);
      debugLog(`CLIENT: Current location: ${data.systemName} (${data.systemID})`);
      return data; // Return location data for use
    } else {
      console.warn("CLIENT: Location data missing systemID or systemName:", data);
      hideLocationDisplay();
      return null;
    }
  } catch (error) {
    console.error("CLIENT: Error fetching current location:", error);
    hideLocationDisplay();
    return null;
  }
}

// Show location display (current system name, optionally as link to zkillboard)
function showLocationDisplay(systemName, systemID) {
  const locationDisplay = document.getElementById("location-display");
  const locationRow = document.getElementById("location-display-location");
  const bannerRow = document.getElementById("location-display-banner");
  const locationText = document.getElementById("current-location-text");

  if (locationDisplay && locationText) {
    if (systemID) {
      currentLocationData = { systemID, systemName };
      const a = document.createElement("a");
      a.href = `https://zkillboard.com/system/${systemID}/`;
      a.target = "_blank";
      a.rel = "noopener";
      a.textContent = systemName;
      a.title = "View on zKillboard";
      locationText.innerHTML = "";
      locationText.appendChild(a);
    } else {
      locationText.innerHTML = "";
      locationText.textContent = systemName;
    }
    if (locationRow) locationRow.style.display = "";
    if (bannerRow) bannerRow.style.display = "none";
    locationDisplay.style.display = "block";
  }
}

const PROXIMITY_LOGIN_BANNER_DEFAULT = "Please log in to use proximity mode with your current location.";

// Show banner in location area (e.g. "Please log in to use proximity mode")
function showProximityLoginBanner(message, showLoginLink = true) {
  const locationDisplay = document.getElementById("location-display");
  const locationRow = document.getElementById("location-display-location");
  const bannerRow = document.getElementById("location-display-banner");
  const bannerText = document.getElementById("location-display-banner-text");
  const loginLink = document.getElementById("location-display-login-link");

  if (!locationDisplay || !bannerRow || !bannerText) return;

  const text = (message && String(message).trim()) ? message : PROXIMITY_LOGIN_BANNER_DEFAULT;
  bannerText.textContent = text;
  if (loginLink) {
    loginLink.style.display = showLoginLink ? "inline" : "none";
    if (showLoginLink) loginLink.href = Config.endpoints.authLogin;
  }
  if (locationRow) locationRow.style.display = "none";
  bannerRow.style.display = "block";
  locationDisplay.style.display = "block";
}

// Hide location display (and banner)
function hideLocationDisplay() {
  const locationDisplay = document.getElementById("location-display");
  if (locationDisplay) {
    locationDisplay.style.display = "none";
  }
}

async function handleModeChange() {
  const modeTabs = document.querySelector(".mode-tabs");
  const modeInputs = document.querySelectorAll("input[name='mode']");
  try {
    if (modeTabs) modeTabs.classList.add("loading");
    modeInputs.forEach(input => { input.disabled = true; });

  // Save the selected mode to localStorage
  const checkedMode = document.querySelector("input[name='mode']:checked");
  const mode = checkedMode ? checkedMode.value : null;
  if (mode) {
    localStorage.setItem('evePvpSearchMode', mode);
  }

  // Don't display results from another mode: hide the table container
  const resultContainer = document.getElementById("result-container");
  if (resultContainer) {
    resultContainer.style.display = "none";
  }

  const jumpCloneStations = document.getElementById("jump-clone-stations");

  // Show/hide highsec checkbox based on mode
  const militiaHighsecLabel = document.getElementById("militia-highsec-label");
  if (mode === "near_trade_hubs" || mode === "proximity") {
    applyMilitiaFilter();
  } else {
    if (militiaHighsecLabel) militiaHighsecLabel.style.display = "none";
  }

  // Configure controls based on mode
  if (mode === "near_trade_hubs") {
    if (jumpCloneStations) jumpCloneStations.style.display = "block"; // Show jump clone stations list in trade hubs mode
    hideLocationDisplay();

    // Always fetch fresh data for trade hubs mode
    if (initialLoadDone) {
      await handleCheckClick();
    } else {
      // Initial load - check if we have server-rendered table
      const resultTable = document.getElementById("result");
      const resultTbody = resultTable && resultTable.querySelector("tbody");
      const hasServerTable = resultTbody && resultTbody.querySelectorAll("tr").length > 0;
      if (hasServerTable) {
        ensureTableExists();
        const resultThead = resultTable.querySelector("thead");
        if (resultThead && resultTbody) {
          modeResultsCache[mode] = {
            thead: resultThead.innerHTML,
            tbody: resultTbody.innerHTML
          };
          applySecurityStatusClasses();
          initializeSorting();
          applySecurityFilters();
          bindWaypointButtons();
          const isAuthenticated = applyAuthFromPage();
          if (isAuthenticated) showSetDestinationButtons();
          else hideSetDestinationButtons();
        }
        const rc = document.getElementById("result-container");
        if (rc) rc.style.display = "flex";
      } else {
        ensureTableExists();
        const rc = document.getElementById("result-container");
        if (rc) rc.style.display = "flex";
      }
    }
  } else {
    // Proximity mode
    if (jumpCloneStations) jumpCloneStations.style.display = "none"; // Hide jump clone stations list in Proximity mode

    // Always fetch fresh data for proximity mode
    showProximityLoginBanner("Please log in to use proximity mode with your current location.", false);
    const isAuthenticated = applyAuthFromPage();
    if (isAuthenticated) {
      hideLocationDisplay();
      if (currentLocationData) showLocationDisplay(currentLocationData.systemName, currentLocationData.systemID);
      await handleCheckClick();
    } else {
      ensureTableExists();
      ensureCcpFooterExists();
      const resultTable = document.getElementById("result");
      if (resultTable) {
        const thead = resultTable.querySelector("thead");
        const tbody = resultTable.querySelector("tbody");
        if (thead) thead.innerHTML = "";
        if (tbody) tbody.innerHTML = "";
      }
      const rc = document.getElementById("result-container");
      if (rc) rc.style.display = "flex";
    }
  }
  } finally {
    modeInputs.forEach(input => { input.disabled = false; });
    if (modeTabs) modeTabs.classList.remove("loading");
  }
}



/** Reset page and table-container scroll so the table is shown from the top. Call after DOM updates. */
function scrollTableToTop(container) {
  if (container) {
    container.scrollTop = 0;
  }
  window.scrollTo({ top: 0, left: 0, behavior: 'auto' });
}

function ensureCcpFooterExists() {
  // Footer at bottom of table (inside result-container); visible when table is empty too
  if (document.getElementById("ccp-disclaimer")) return;
  const container = document.getElementById("result-container");
  if (!container) return;
  const footer = document.createElement("footer");
  footer.id = "ccp-disclaimer";
  footer.textContent = "© 2014 CCP hf. All rights reserved. EVE Online and the EVE logo are trademarks or registered trademarks of CCP hf. All rights are reserved worldwide. All other trademarks are the property of their respective owners. EVE Online, the EVE logo, EVE and all associated logos and designs are the intellectual property of CCP hf. All artwork, screenshots, characters, vehicles, storylines, world facts or other recognizable features of the intellectual property relating to these trademarks are likewise the intellectual property of CCP hf. CCP hf. has granted permission to Eve PvP Radar to use EVE Online and all associated logos and designs for promotional and informational purposes on its website but does not endorse, and is not in any way affiliated with, Eve PvP Radar. CCP is in no way responsible for the content on or functioning of this website, nor can it be liable for any damage arising from the use of this website.";
  container.appendChild(footer);
}

function ensureTableExists() {
  // Create table if it doesn't exist
  let resultTable = document.getElementById("result");
  if (!resultTable) {
    // Check if container already exists
    let container = document.getElementById("result-container");
    let createdContainer = false;
    if (!container) {
      container = document.createElement("div");
      container.id = "result-container";
      createdContainer = true;
    }
    
    const table = document.createElement("table");
    table.id = "result";
    const thead = document.createElement("thead");
    const tbody = document.createElement("tbody");
    tbody.id = "notes";
    table.appendChild(thead);
    table.appendChild(tbody);
    // Insert table before footer so footer stays at bottom (container may have footer from server empty state)
    const existingFooter = container.querySelector("#ccp-disclaimer");
    container.insertBefore(table, existingFooter);
    ensureCcpFooterExists();
    const securityFilters = document.getElementById("security-filters");
    // Keep existing DOM order (Thera camps above result container) when container already exists.
    // Only place the container in the layout when we created it from scratch.
    if (createdContainer && securityFilters && securityFilters.parentElement) {
      securityFilters.parentElement.insertBefore(container, securityFilters.nextSibling);
    }
    } else {
    // Table exists, ensure it's in a container and has footer
    let container = document.getElementById("result-container");
    if (!container) {
      container = document.createElement("div");
      container.id = "result-container";
      resultTable.parentElement.insertBefore(container, resultTable);
      container.appendChild(resultTable);
    }
    ensureCcpFooterExists();
  }
}

function escapeHtml(s) {
  const div = document.createElement("div");
  div.textContent = s;
  return div.innerHTML;
}
function escapeAttr(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function clearDestinationSuccessState(button) {
  if (!button) return;
  button.textContent = "Set destination";
  button.style.backgroundColor = "";
  button.style.color = "";
  button.disabled = false;
  button.removeAttribute("data-success");
}

function clearAllDestinationSuccessStates(exceptButton) {
  const buttons = document.querySelectorAll(".set-destination-btn, .jump-clone-destination-btn");
  buttons.forEach(button => {
    if (button !== exceptButton && button.getAttribute("data-success") === "true") {
      clearDestinationSuccessState(button);
    }
  });
}


async function fetchNearTradeHubs() {
  // Near trade hubs mode: fetch trade hubs with target systems that have recent kills
  debugLog("CLIENT: fetchNearTradeHubs called");
  debugLog("CLIENT: Fetching near trade hubs");
  try {
    // Fetch HTML table from server
    const response = await fetch(Config.endpoints.nearTradeHubs);
    if (!response.ok) {
      throw new Error("Fetch failed");
    }
    const html = await response.text();
    
    // Ensure table exists before using it
    ensureTableExists();
    
    // Parse the HTML and extract table content
    // Try parsing as full HTML document first
    const parser = new DOMParser();
    let doc = parser.parseFromString(html, "text/html");
    
    let newTable = doc.querySelector('table') || (doc.body && doc.body.querySelector('table'));
    
    // If table not found, try parsing as a table fragment by wrapping it
    if (!newTable) {
      const wrappedHtml = `<html><body>${html}</body></html>`;
      doc = parser.parseFromString(wrappedHtml, "text/html");
      newTable = doc.querySelector('table');
    }
    
    if (!newTable) {
      console.error("CLIENT: No table found in HTML response");
      ensureTableExists();
      const container = document.getElementById("result-container");
      if (container) container.style.display = "flex";
      return;
    }
    
    const resultTable = document.getElementById("result");
    if (!resultTable) return;
    
    const container = document.getElementById("result-container");
    
    // Update table headers
    const newThead = newTable.querySelector('thead');
    const resultThead = resultTable.querySelector('thead');
    if (newThead && resultThead) {
      resultThead.innerHTML = newThead.innerHTML;
    }
    
    // Replace tbody content
    const newTbody = newTable.querySelector('tbody');
    const resultTbody = resultTable.querySelector('tbody');
    if (newTbody && resultTbody) {
      const rowCount = newTbody.querySelectorAll('tr').length;
      
      // Check if there are any data rows
      if (rowCount === 0) {
        // Show container with empty table and CCP footer
        if (container) container.style.display = "flex";
        clearPilotOnlyFilter();
        resultTbody.innerHTML = newTbody.innerHTML;
        modeResultsCache.near_trade_hubs = null;
      } else {
        // Show container and update tbody content
        // Clear any hash from URL to prevent scrolling to anchors
        if (window.location.hash) {
          history.replaceState(null, null, window.location.pathname + window.location.search);
        }
        if (container) container.style.display = "flex";
        clearPilotOnlyFilter();
        resultTbody.innerHTML = newTbody.innerHTML;
        modeResultsCache.near_trade_hubs = {
          thead: resultThead ? resultThead.innerHTML : "",
          tbody: resultTbody.innerHTML
        };
      }
    } else {
      console.error("CLIENT: No tbody found in HTML response");
      if (container) container.style.display = "flex";
      modeResultsCache.near_trade_hubs = null;
    }
    
    // Re-bind event handlers for route display (already handled by event delegation)
    ensureShipTypeIconsUseWikiLinks(document.getElementById("thera-camps-container"));
    // Apply security status classes and filters
    applySecurityStatusClasses();
    
    // Initialize sorting
    initializeSorting();
    
    applySecurityFilters();
    
    // Bind waypoint buttons and update visibility based on auth status
    bindWaypointButtons();
    // Wait for auth status check to complete before showing/hiding buttons
    const isAuthenticated = applyAuthFromPage();
    if (isAuthenticated) {
      showSetDestinationButtons();
    } else {
      hideSetDestinationButtons();
    }

  } catch (error) {
    console.error("CLIENT: Fetch error for near trade hubs:", error);
    ensureTableExists();
    const container = document.getElementById("result-container");
    if (container) container.style.display = "flex";
  }
}

async function fetchProximity() {
  debugLog("CLIENT: Fetching proximity systems for current character location");
  try {
    const response = await fetch(Config.endpoints.proximity, {
      credentials: 'include'
    });
    if (response.status === 401) {
      showProximityLoginBanner("Please log in to use proximity mode with your current location.", false);
      modeResultsCache.proximity = null;
      ensureTableExists();
      const container = document.getElementById("result-container");
      if (container) container.style.display = "flex";
      return;
    }
    if (!response.ok) {
      throw new Error("Fetch failed");
    }
    const html = await response.text();
    
    // Ensure table exists before using it
    ensureTableExists();
    
    // Parse the HTML and extract proximity-meta (character location) and table content
    const parser = new DOMParser();
    const doc = parser.parseFromString(html, "text/html");
    const metaDiv = doc.getElementById("proximity-meta");
    let startSystemId = null;
    let startSystemName = null;
    if (metaDiv) {
      startSystemId = parseInt(metaDiv.getAttribute("data-system-id"), 10);
      startSystemName = metaDiv.getAttribute("data-system-name");
      if (startSystemId && startSystemName) {
        showLocationDisplay(startSystemName, startSystemId);
      }
    }
    const newTable = doc.querySelector('table') || (doc.body && doc.body.querySelector('table'));
    
    if (!newTable) {
      console.error("CLIENT: No table found in HTML response");
      ensureTableExists();
      const container = document.getElementById("result-container");
      if (container) container.style.display = "flex";
      return;
    }
    
    const resultTable = document.getElementById("result");
    if (!resultTable) return;
    
    const container = document.getElementById("result-container");
    
    // Update table headers
    const newThead = newTable.querySelector('thead');
    const resultThead = resultTable.querySelector('thead');
    if (newThead && resultThead) {
      resultThead.innerHTML = newThead.innerHTML;
    }
    
    // Replace tbody content
    const newTbody = newTable.querySelector('tbody');
    const resultTbody = resultTable.querySelector('tbody');
    if (newTbody && resultTbody) {
      const rowCount = newTbody.querySelectorAll('tr').length;
      
      // Check if there are any data rows
      if (rowCount === 0) {
        // Show container with empty table and CCP footer
        if (container) container.style.display = "flex";
        clearPilotOnlyFilter();
        resultTbody.innerHTML = newTbody.innerHTML;
        modeResultsCache.proximity = null;
      } else {
        // Show container and update tbody content
        // Clear any hash from URL to prevent scrolling to anchors
        if (window.location.hash) {
          history.replaceState(null, null, window.location.pathname + window.location.search);
        }
        if (container) container.style.display = "flex";
        clearPilotOnlyFilter();
        resultTbody.innerHTML = newTbody.innerHTML;
        modeResultsCache.proximity = {
          thead: resultThead ? resultThead.innerHTML : "",
          tbody: resultTbody.innerHTML,
          systemId: startSystemId || undefined
        };
      }
    } else {
      console.error("CLIENT: No tbody found in HTML response");
      if (container) container.style.display = "flex";
      modeResultsCache.proximity = null;
    }

    ensureCcpFooterExists();

    // Re-bind event handlers for route display (already handled by event delegation)
    ensureShipTypeIconsUseWikiLinks(document.getElementById("thera-camps-container"));
    // Apply security status classes and filters
    applySecurityStatusClasses();
    
    // Initialize sorting
    initializeSorting();
    
    applySecurityFilters();
    
    // Bind waypoint buttons and update visibility based on auth status
    bindWaypointButtons();
    // Wait for auth status check to complete before showing/hiding buttons
    const isAuthenticated = applyAuthFromPage();
    if (isAuthenticated) {
      showSetDestinationButtons();
    } else {
      hideSetDestinationButtons();
    }

  } catch (error) {
    console.error("CLIENT: Fetch error for proximity systems:", error);
    ensureTableExists();
    const container = document.getElementById("result-container");
    if (container) container.style.display = "flex";
  }
}

function bindAttackersToggleHandlers() {
  // Legacy function kept for API compatibility; event delegation handles this now.
}

async function handleCheckClick() {
  const checkedMode = document.querySelector("input[name='mode']:checked");
  const mode = checkedMode ? checkedMode.value : null;
    debugLog("CLIENT: Mode:", mode);
    if (mode === "near_trade_hubs") {
      await fetchNearTradeHubs()
        .catch(error => {
          console.error("CLIENT: Error fetching near trade hubs:", error);
        });
    } else {
      // Proximity mode - backend fetches character location from ESI and returns proximity data
      await fetchProximity()
        .catch(error => {
          console.error("CLIENT: Error fetching proximity systems:", error);
        });
    }
}

// Security color mapping based on security values
const securityColorMap = {
  1.0: "#2E74DF",
  0.9: "#3B9CEC",
  0.8: "#49D0F1",
  0.7: "#5CDCA6",
  0.6: "#72E352",
  0.5: "#EEFF83",
  0.4: "#E06A0B",
  0.3: "#CE4610",
  0.2: "#BC1211",
  0.1: "#6C2222",
  0.0: "#8D3263"
};

/** Same as backend displayEveSecurityForUI: 0 < truesec < 0.1 → display 0.1 lowsec. */
function displayEveSecurityForUI(s) {
  if (s < 0) return 0;
  if (s === 0 || Object.is(s, -0)) return 0;
  if (s < 0.1) return 0.1;
  return Math.round(s * 10) / 10;
}

// Get color for a security value with interpolation
function getSecurityColor(securityValue) {
  const displayed = displayEveSecurityForUI(securityValue);
  const clamped = Math.max(0.0, displayed);
  
  // Round to nearest 0.1 for lookup
  const rounded = Math.round(clamped * 10) / 10;
  
  // If exact match, return it
  if (securityColorMap.hasOwnProperty(rounded)) {
    return securityColorMap[rounded];
  }
  
  // Interpolate between nearest two values
  const lower = Math.floor(clamped * 10) / 10;
  const upper = Math.ceil(clamped * 10) / 10;
  
  // Ensure bounds
  const lowerKey = Math.max(0.0, lower);
  const upperKey = Math.min(1.0, upper);
  
  if (lowerKey === upperKey) {
    return securityColorMap[lowerKey] || securityColorMap[0.0];
  }
  
  const lowerColor = securityColorMap[lowerKey] || securityColorMap[0.0];
  const upperColor = securityColorMap[upperKey] || securityColorMap[1.0];
  
  // Interpolate between colors
  const t = (clamped - lowerKey) / (upperKey - lowerKey);
  return interpolateColor(lowerColor, upperColor, t);
}

// Interpolate between two hex colors
function interpolateColor(color1, color2, t) {
  const rgb1 = hexToRgb(color1);
  const rgb2 = hexToRgb(color2);
  
  if (!rgb1 || !rgb2) return color1;
  
  const r = Math.round(rgb1.r + (rgb2.r - rgb1.r) * t);
  const g = Math.round(rgb1.g + (rgb2.g - rgb1.g) * t);
  const b = Math.round(rgb1.b + (rgb2.b - rgb1.b) * t);
  
  return `#${[r, g, b].map(x => {
    const hex = x.toString(16);
    return hex.length === 1 ? '0' + hex : hex;
  }).join('')}`;
}

// Convert hex to RGB
function hexToRgb(hex) {
  const result = /^#?([a-f\d]{2})([a-f\d]{2})([a-f\d]{2})$/i.exec(hex);
  return result ? {
    r: parseInt(result[1], 16),
    g: parseInt(result[2], 16),
    b: parseInt(result[3], 16)
  } : null;
}

// Apply security status CSS classes and colors to table rows and route systems
function applySecurityStatusClasses() {
  const resultTable = document.getElementById("result");
  if (!resultTable) return;

  const rows = resultTable.querySelectorAll("tbody tr");
  rows.forEach(row => {
    // Remove existing security classes
    row.classList.remove("system-lowsec", "system-nullsec", "system-highsec");
    
    // Get security value from the system cell (first cell with data-security)
    const systemCell = row.querySelector("td[data-security]");
    if (!systemCell) return;
    
    const securityValue = parseFloat(systemCell.getAttribute("data-security"));
    if (isNaN(securityValue)) return;
    
    // Get color from mapping
    const color = getSecurityColor(securityValue);
    
    // Apply background color to row (with transparency)
    const rgb = hexToRgb(color);
    if (rgb) {
      // Use different opacity based on security type
      if (securityValue <= 0.0) {
        row.style.backgroundColor = `rgba(${rgb.r}, ${rgb.g}, ${rgb.b}, 0.2)`;
        row.classList.add("system-nullsec");
      } else if (securityValue < 0.45) {
        row.style.backgroundColor = `rgba(${rgb.r}, ${rgb.g}, ${rgb.b}, 0.15)`;
        row.classList.add("system-lowsec");
      } else {
        row.style.backgroundColor = "";
        row.classList.add("system-highsec");
      }
    }
  });

  // Colorize route system security numbers based on security status
  const routeSecurityNumbers = resultTable.querySelectorAll(".route-security-number");
  routeSecurityNumbers.forEach(securityNumber => {
    // Get security value from data attribute (original value, not displayed value)
    const securityValue = parseFloat(securityNumber.getAttribute("data-security"));
    if (!isNaN(securityValue)) {
      const color = getSecurityColor(securityValue);
      securityNumber.style.color = color;
    }
  });
}

// Derive pilot icons from the server-injected templates (embedded from backend/static/*.svg at build time).
// This avoids front-end SVG drift vs backend renderKillmailHTML.
const pilotRunningSVGEl = document.getElementById("pilot-running-icon-template");
const pilotStandingSVGEl = document.getElementById("pilot-standing-icon-template");
const pilotRunningSVG = pilotRunningSVGEl ? (pilotRunningSVGEl.innerHTML || "").trim() : "";
const pilotStandingSVG = pilotStandingSVGEl ? (pilotStandingSVGEl.innerHTML || "").trim() : "";

/** When non-empty, only table rows (systems) where at least one of these pilots appears as an attacker stay visible. */
const pilotOnlyCharacterIds = new Set();

/** When a trade hub name is in this set, its rows are hidden in the main table. */
let hiddenTradeHubs = new Set();

/** Max attackers limit. -1 means unbound (no filtering). 1-100 means hide rows with attacker count > this value. */
let maxAttackersLimit = -1;

const filterStateKey = 'evePvpSearchFilters';
function saveFilterState() {
  const filterLowsec = document.getElementById("filter-lowsec");
  const filterNullsec = document.getElementById("filter-nullsec");
  const filterGatecamps = document.getElementById("filter-gatecamps");
  const state = {
    filterLowsec: filterLowsec ? filterLowsec.checked : true,
    filterNullsec: filterNullsec ? filterNullsec.checked : true,
    filterGatecamps: filterGatecamps ? filterGatecamps.checked : false,
    maxAttackers: maxAttackersLimit,
    hiddenTradeHubs: Array.from(hiddenTradeHubs).map(h => String(h).toLowerCase())
  };
  try {
    localStorage.setItem(filterStateKey, JSON.stringify(state));
  } catch (e) {}
}

function restoreFilterState() {
  try {
    const saved = localStorage.getItem(filterStateKey);
    if (!saved) return;
    const state = JSON.parse(saved);
    const filterLowsec = document.getElementById("filter-lowsec");
    const filterNullsec = document.getElementById("filter-nullsec");
    const filterGatecamps = document.getElementById("filter-gatecamps");
    if (filterLowsec && state.filterLowsec !== undefined) filterLowsec.checked = state.filterLowsec;
    if (filterNullsec && state.filterNullsec !== undefined) filterNullsec.checked = state.filterNullsec;
    if (filterGatecamps && state.filterGatecamps !== undefined) filterGatecamps.checked = state.filterGatecamps;
    if (state.maxAttackers !== undefined) {
      maxAttackersLimit = state.maxAttackers;
      const display = document.getElementById("max-attackers-display");
      if (display) {
        display.textContent = maxAttackersLimit < 1 ? "unbound" : String(maxAttackersLimit);
      }
    }
    if (Array.isArray(state.hiddenTradeHubs)) {
      hiddenTradeHubs = new Set(state.hiddenTradeHubs.map(h => String(h).toLowerCase()));
      syncTradeHubCheckboxes();
    }
    applySecurityFilters();
  } catch (e) {}
}

function ensureGatecampFilterRunningIcon() {
  const runningIconHost = document.getElementById("filter-gatecamps-running-icon");
  if (!runningIconHost) return;
  if (runningIconHost.dataset.iconReady === "true") return;
  runningIconHost.innerHTML = pilotRunningSVG || "";
  runningIconHost.dataset.iconReady = "true";
}

function buildGatecampHintTooltip(hiddenSystems) {
  const base = "Warning: gatecamp filtering is currently hiding systems for your selected pilot filter.";
  if (!hiddenSystems || hiddenSystems.length === 0) return base;
  return `${base}\nHidden systems: ${hiddenSystems.join(", ")}`;
}

function getRowSystemDisplayName(row) {
  if (!row) return "";
  const dataName = (row.getAttribute("data-system-name") || "").trim();
  if (dataName) return dataName;

  const firstCell = row.querySelector("td:first-child");
  if (!firstCell) return "";
  const link = firstCell.querySelector("a");
  const rawText = (link ? link.textContent : firstCell.textContent || "").trim();
  if (!rawText) return "";

  // First cell often contains "<SystemName> (<sec>)". Keep only the system name.
  return rawText.replace(/\s*\([^)]*\)\s*$/, "").trim();
}

function updateGatecampFilterHintIconsVisibility(show, hiddenSystems) {
  const hintIcons = document.getElementById("filter-gatecamps-hint-icons");
  const warningIcon = document.getElementById("filter-gatecamps-warning-icon");
  const runningIcon = document.getElementById("filter-gatecamps-running-icon-wrap");
  if (!hintIcons) return;
  hintIcons.classList.toggle("is-visible", !!show);
  if (show) {
    ensureGatecampFilterRunningIcon();
    const tooltip = buildGatecampHintTooltip(hiddenSystems);
    if (warningIcon) warningIcon.setAttribute("title", tooltip);
    if (runningIcon) runningIcon.setAttribute("title", tooltip);
  } else {
    if (warningIcon) warningIcon.removeAttribute("title");
    if (runningIcon) runningIcon.removeAttribute("title");
  }
}

function clearPilotOnlyFilter() {
  pilotOnlyCharacterIds.clear();
}

function ensureShipTypeIconsUseWikiLinks(container) {
  if (!container) return;

  // Safety net: ensure every ship icon in Thera camps resolves to EVE Uni.
  // This covers cases where server/client markup updates accidentally place
  // the icon inside a zKillboard link.
  const icons = container.querySelectorAll(".ship-type-icon");
  icons.forEach((icon) => {
    // Already correctly wrapped.
    const existingWikiLink = icon.closest("a[href*='wiki.eveuniversity.org']");
    if (existingWikiLink) return;

    // Derive ship name from nearby text.
    const host = icon.closest("a, span, div") || icon.parentElement;
    const shipText = host ? host.querySelector(".ship-type-text") : null;
    const shipName = shipText ? (shipText.textContent || "").trim() : "";
    if (!shipName || shipName === "Unknown ship") return;

    const wikiTitle = shipName.replace(/\s+/g, "_");
    const wikiURL = `https://wiki.eveuniversity.org/${encodeURIComponent(wikiTitle)}`;

    const wikiLink = document.createElement("a");
    wikiLink.href = wikiURL;
    wikiLink.target = "_blank";
    wikiLink.rel = "noopener noreferrer";
    wikiLink.className = "ship-type-icon-link";

    const currentLink = icon.closest("a");
    if (currentLink) {
      currentLink.parentNode.insertBefore(wikiLink, currentLink);
    } else {
      icon.parentNode.insertBefore(wikiLink, icon);
    }
    wikiLink.appendChild(icon);
  });
}

function buildPilotMultiSystemTooltip(systemSet) {
  if (!systemSet || systemSet.size <= 1) return "";
  return "Pilot appears in multiple systems";
}

/** Base pilot tooltip; preserves ESI failure detail from server HTML. */
function getPilotBaseTooltip(link, pilotName) {
  if (link.getAttribute("data-pilot-unresolved") === "true") {
    return (link.getAttribute("data-tooltip") || pilotName).trim();
  }
  return pilotName;
}

function ensurePilotIconWrapContent(link, iconHTML, id) {
  // Multi-system icon is only visible inside the multi wrapper.
  // When the multi wrapper exists but the icon span is missing (can happen
  // when JS flips single->multi after filters), create it.
  const wrap = link.closest("span.pilot-multi-system");
  if (!wrap) return;

  let iconWrap = wrap.querySelector(`.pilot-icon-wrap[data-character-id='${id}']`);
  if (!iconWrap) {
    iconWrap = wrap.querySelector(".pilot-icon-wrap");
  }

  if (!iconWrap) {
    iconWrap = document.createElement("span");
    iconWrap.className = "pilot-icon-wrap";
    iconWrap.setAttribute("data-character-id", id);
    // Insert right after ship-type link.
    wrap.insertBefore(iconWrap, link.nextSibling);
  } else {
    iconWrap.setAttribute("data-character-id", id);
  }

  iconWrap.innerHTML = iconHTML;
}

/** Adjust running/standing pilot icons from rows that are actually visible (security filters use display:none). */
function syncPilotMultiSystemHighlights() {
  const table = document.getElementById("result");
  if (!table) return;
  // Without data-attacker-count (older server HTML), we cannot mirror the server's ≤5-attacker rule — skip.
  if (!table.querySelector(".killmail-row[data-attacker-count]")) return;

  const charToSystems = new Map();
  // If pilot-only filtering is active, we intentionally include *all* systems in the map.
  // Otherwise, hiding rows would make some pilots look single-system and flip running->standing icons.
  const rowSelector = pilotOnlyCharacterIds.size > 0 ? "tbody tr" : "tbody tr:not(.filtered-out)";
  for (const tr of table.querySelectorAll(rowSelector)) {
    const sys = tr.getAttribute("data-system");
    if (!sys) continue;
    for (const km of tr.querySelectorAll(".killmail-row[data-attacker-count]")) {
      // Count pilots regardless of attacker count so repeat pilots get the correct icon.
      // (Icons are shown inside both collapsed and expanded attacker blocks.)
      const n = parseInt(km.getAttribute("data-attacker-count") || "0", 10);
      for (const a of km.querySelectorAll("a[href*='/character/']")) {
        const href = a.getAttribute("href") || "";
        const m = href.match(/\/character\/(\d+)\//);
        if (!m) continue;
        const id = m[1];
        if (!charToSystems.has(id)) charToSystems.set(id, new Set());
        charToSystems.get(id).add(sys);
      }
    }
  }

  const wantMulti = (id) => {
    if (pilotOnlyCharacterIds.has(id)) return true;
    const s = charToSystems.get(id);
    return !!(s && s.size > 1);
  };

  const links = Array.from(table.querySelectorAll("tbody a[href*='/character/']"));
  for (const link of links) {
    const href = link.getAttribute("href") || "";
    const m = href.match(/\/character\/(\d+)\//);
    if (!m) continue;

    const id = m[1];
    const multi = wantMulti(id);
    const inMulti = !!link.closest("span.pilot-multi-system");

    if (multi) {
      if (!inMulti) {
        const span = document.createElement("span");
        span.className = "pilot-multi-system";
        const parent = link.parentNode;
        const iconWrap = parent ? parent.querySelector(`.pilot-icon-wrap[data-character-id='${id}']`) : null;
        parent.insertBefore(span, link);
        span.appendChild(link);
        if (iconWrap) span.appendChild(iconWrap);
      }
      const wrap = link.closest("span.pilot-multi-system");
      // Ensure the running-man icon exists and acts as the filter toggle.
      ensurePilotIconWrapContent(link, pilotRunningSVG, id);

      if (wrap) {
        const iconWrap = wrap.querySelector(`.pilot-icon-wrap[data-character-id='${id}']`) || wrap.querySelector(".pilot-icon-wrap");
        if (iconWrap) {
          iconWrap.classList.add("pilot-hide-systems-btn");
          iconWrap.dataset.characterId = id;
          iconWrap.setAttribute("role", "button");
          iconWrap.setAttribute("tabindex", "0");
        }
        const pressed = pilotOnlyCharacterIds.has(id);
        if (iconWrap) {
          iconWrap.setAttribute("aria-pressed", pressed ? "true" : "false");
          iconWrap.classList.toggle("active", pressed);
          iconWrap.setAttribute("title", pressed ? "Clear filter (show all systems again)" : "Show only systems where this pilot appears");
          iconWrap.setAttribute("aria-label", pressed ? "Pilot filter active. Click to show all systems again." : "Show only systems where this pilot appears as an attacker");
        }
      }
      const pilotName = (link.getAttribute("data-pilot-name") || "Pilot").trim();
      if (!link.getAttribute("data-pilot-name")) {
        link.setAttribute("data-pilot-name", pilotName);
      }
      const baseTooltip = getPilotBaseTooltip(link, pilotName);
      const displayLabel = link.getAttribute("data-pilot-unresolved") === "true" ? baseTooltip : pilotName;
      let extra = buildPilotMultiSystemTooltip(charToSystems.get(id));
      if (!extra && pilotOnlyCharacterIds.has(id)) {
        extra = "Pilot appears in multiple systems";
      }
      link.setAttribute("data-tooltip", extra ? `${baseTooltip}. ${extra}` : baseTooltip);
      link.setAttribute("aria-label", extra ? `${displayLabel}. ${extra}` : displayLabel);
    } else {
      if (inMulti) {
        const span = link.closest("span.pilot-multi-system");
        const parent = span && span.parentNode;
        if (parent) {
          // Only move the standalone icon span (new markup). For old markup the icon lives inside the <a>.
          const iconWrap = span.querySelector(`.pilot-icon-wrap[data-character-id='${id}']`);
          const afterSpan = span.nextSibling;
          parent.insertBefore(link, afterSpan);
          if (iconWrap) parent.insertBefore(iconWrap, afterSpan);
          span.remove();
        }
      }
      const iconWrap = link.parentElement ? link.parentElement.querySelector(`.pilot-icon-wrap[data-character-id='${id}']`) : null;
      if (iconWrap) {
        iconWrap.classList.remove("pilot-hide-systems-btn", "active");
        iconWrap.removeAttribute("data-character-id");
        iconWrap.removeAttribute("aria-pressed");
        iconWrap.removeAttribute("title");
        iconWrap.removeAttribute("aria-label");
        iconWrap.removeAttribute("role");
        iconWrap.removeAttribute("tabindex");
      }
      const pilotName = (link.getAttribute("data-pilot-name") || "Pilot").trim();
      const baseTooltip = getPilotBaseTooltip(link, pilotName);
      const displayLabel = link.getAttribute("data-pilot-unresolved") === "true" ? baseTooltip : pilotName;
      link.setAttribute("data-tooltip", baseTooltip);
      link.setAttribute("aria-label", displayLabel);
    }
  }
}

// Clear all filters and show all data
function clearAllFilters() {
  document.querySelectorAll(".pilot-hide-systems-btn").forEach(function(btn) {
    btn.classList.remove("active");
    btn.setAttribute("aria-pressed", "false");
    btn.setAttribute("title", "Show only systems where this pilot appears");
    btn.setAttribute("aria-label", "Show only systems where this pilot appears as an attacker");
  });
  pilotOnlyCharacterIds.clear();
  hiddenTradeHubs.clear();
  syncTradeHubCheckboxes();
  document.querySelectorAll(".trade-hub-filter-btn").forEach(function(btn) {
    btn.classList.remove("active");
    btn.setAttribute("aria-pressed", "false");
  });
  var filterLowsec = document.getElementById("filter-lowsec");
  var filterNullsec = document.getElementById("filter-nullsec");
  var filterGatecamps = document.getElementById("filter-gatecamps");
  if (filterLowsec) filterLowsec.checked = true;
  if (filterNullsec) filterNullsec.checked = true;
  if (filterGatecamps) filterGatecamps.checked = false;
  setMaxAttackersLimit(-1);
  saveFilterState();
}

// Update the filter indicator icon at the top-left corner of the table
function updateFilterIndicator(counts) {
  let indicator = document.getElementById("filter-indicator");
  if (!indicator) {
    const firstTh = document.querySelector("#result thead th");
    if (!firstTh) return;
    indicator = document.createElement("div");
    indicator.id = "filter-indicator";
    const filterTemplate = document.getElementById("pilot-filter-icon-template");
    const filterSvg = filterTemplate ? (filterTemplate.innerHTML || "").trim() : "";
    const iconSpan = document.createElement("span");
    iconSpan.innerHTML = filterSvg || '<svg xmlns="http://www.w3.org/2000/svg" viewBox="8 8 48 48"><path d="m8 8 18.667 21.333v21.333l10.667 5.333V29.332L56.001 7.999h-24z"/></svg>';
    indicator.appendChild(iconSpan);
    const tooltip = document.createElement("div");
    tooltip.id = "filter-indicator-tooltip";
    indicator.appendChild(tooltip);
    firstTh.insertBefore(indicator, firstTh.firstChild);
  }
  const tooltip = document.getElementById("filter-indicator-tooltip");
  if (!tooltip) return;
  const items = [];
  if (counts.lowsec > 0) items.push("Lowsec: " + counts.lowsec + " system" + (counts.lowsec !== 1 ? "s" : "") + " hidden");
  if (counts.nullsec > 0) items.push("Nullsec: " + counts.nullsec + " system" + (counts.nullsec !== 1 ? "s" : "") + " hidden");
  if (counts.highsec > 0) items.push("Highsec: " + counts.highsec + " system" + (counts.highsec !== 1 ? "s" : "") + " hidden");
  if (counts.pilot > 0) items.push("Pilot filter: " + counts.pilot + " system" + (counts.pilot !== 1 ? "s" : "") + " hidden");
  if (counts.tradeHub > 0) items.push("Trade hub filter: " + counts.tradeHub + " system" + (counts.tradeHub !== 1 ? "s" : "") + " hidden");
  if (counts.gatecamps > 0) items.push("Gatecamp filter: " + counts.gatecamps + " system" + (counts.gatecamps !== 1 ? "s" : "") + " hidden");
  if (counts.maxAttackers > 0) items.push("Max attackers filter: " + counts.maxAttackers + " system" + (counts.maxAttackers !== 1 ? "s" : "") + " hidden");
  if (items.length > 0) {
    tooltip.innerHTML = items.map(function(t) { return '<div class="filter-indicator-tooltip-item">' + t + "</div>"; }).join("");
    indicator.style.display = "block";
  } else {
    indicator.style.display = "none";
  }
}

document.addEventListener("click", function(e) {
  if (e.target && e.target.closest && e.target.closest("#filter-indicator")) {
    clearAllFilters();
  }
});

// Update max attackers control display and re-apply filters
function setMaxAttackersLimit(value) {
  maxAttackersLimit = value;
  const display = document.getElementById("max-attackers-display");
  if (display) {
    display.textContent = maxAttackersLimit < 1 ? "unbound" : maxAttackersLimit;
  }
  applySecurityFilters();
}

// Apply militia filter: manage highsec checkbox visibility and state.
// Row visibility is handled by CSS :has() rules (no JS needed).
function applyMilitiaFilter() {
  const auth = window.__auth || {};
  const militiaShortName = auth.militiaShortName || "";
  const highsecCheckbox = document.getElementById("militia-highsec");

  // Show/hide the highsec checkbox label based on whether character has militia
  const highsecLabel = document.getElementById("militia-highsec-label");
  if (highsecLabel) {
    highsecLabel.style.display = militiaShortName ? "" : "none";
  }

  // No militia: force-uncheck so CSS rules hide highsec kills
  if (!militiaShortName && highsecCheckbox) {
    highsecCheckbox.checked = false;
  }
}

// Apply security filters based on checkbox states
// Lowsec, nullsec, trade hub, and gatecamp row visibility is handled by CSS :has() rules.
// This function handles pilot-only, max attackers (JS class-based)
// and counts all hidden rows for the filter indicator.
function applySecurityFilters() {
  applyMilitiaFilter();

  const filterLowsec = document.getElementById("filter-lowsec");
  const filterNullsec = document.getElementById("filter-nullsec");
  const filterGatecamps = document.getElementById("filter-gatecamps");
  const showLowsec = filterLowsec ? filterLowsec.checked : true;
  const showNullsec = filterNullsec ? filterNullsec.checked : true;
  const hideGatecamps = filterGatecamps ? filterGatecamps.checked : false;

  const checkedMode = document.querySelector("input[name='mode']:checked");
  const mode = checkedMode ? checkedMode.value : null;
  const hasHiddenHubs = mode === "near_trade_hubs" && hiddenTradeHubs.size > 0;

  const resultTable = document.getElementById("result");
  if (!resultTable) {
    updateGatecampFilterHintIconsVisibility(false);
    saveFilterState();
    return;
  }

  // Query rows once; filter/count in JS instead of repeated querySelectorAll.
  const allRows = resultTable.querySelectorAll("tbody tr");
  let hiddenByLowsec = 0;
  let hiddenByNullsec = 0;
  let hiddenByTradeHub = 0;
  let hiddenByHighsec = 0;

  // Count rows hidden by CSS (lowsec, nullsec, trade hubs) for the indicator.
  if (!showLowsec || !showNullsec || hasHiddenHubs) {
    for (let i = 0; i < allRows.length; i++) {
      const row = allRows[i];
      if (!showLowsec && row.getAttribute("data-sec") === "lowsec") hiddenByLowsec++;
      if (!showNullsec && row.getAttribute("data-sec") === "nullsec") hiddenByNullsec++;
      if (hasHiddenHubs) {
        const hubName = (row.getAttribute("data-trade-hub-row") || "").trim().toLowerCase();
        if (hubName && hiddenTradeHubs.has(hubName)) hiddenByTradeHub++;
      }
    }
  }

  // Count system rows hidden by CSS highsec rule (only-highsec-kill rows)
  const highsecCheckbox = document.getElementById("militia-highsec");
  if (highsecCheckbox && !highsecCheckbox.checked) {
    for (let i = 0; i < allRows.length; i++) {
      const row = allRows[i];
      if (row.querySelectorAll('.killmail-row[data-source="highsec-station"]').length > 0 &&
          row.querySelectorAll('.killmail-row:not([data-source="highsec-station"])').length === 0) {
        hiddenByHighsec++;
      }
    }
  }

  if (mode === "near_trade_hubs") {
    resultTable.querySelectorAll(".trade-hub-filter-btn").forEach(btn => {
      const btnTerm = (btn.getAttribute("data-trade-hub-filter") || "").trim().toLowerCase();
      const isActive = hasHiddenHubs && !hiddenTradeHubs.has(btnTerm);
      btn.classList.toggle("active", isActive);
      btn.setAttribute("aria-pressed", isActive ? "true" : "false");
    });
  }

  let hiddenByPilot = 0;
  let hiddenByGatecamps = 0;
  let hiddenByMaxAttackers = 0;
  let gatecampHidSelectedPilotRows = false;
  const hiddenSystemsForSelectedPilots = new Set();

  // Count gatecamp-hidden rows (CSS hides them; count for indicator + pilot hints)
  if (hideGatecamps) {
    for (let i = 0; i < allRows.length; i++) {
      const row = allRows[i];
      const cells = row.querySelectorAll("td");
      for (let j = 0; j < cells.length; j++) {
        if (cells[j].querySelector(".distance-value")) {
          if (cells[j].querySelector(".route-warning-sign")) {
            hiddenByGatecamps++;
            if (pilotOnlyCharacterIds.size > 0) {
              for (const cid of pilotOnlyCharacterIds) {
                if (row.querySelector(`a.pilot-link[data-character-id='${cid}']`)) {
                  gatecampHidSelectedPilotRows = true;
                  const systemName = getRowSystemDisplayName(row);
                  if (systemName) hiddenSystemsForSelectedPilots.add(systemName);
                  break;
                }
              }
            }
          }
          break;
        }
      }
    }
  }

  const jsFiltersActive = maxAttackersLimit >= 1 || pilotOnlyCharacterIds.size > 0;

  if (!jsFiltersActive) {
    for (let i = 0; i < allRows.length; i++) {
      allRows[i].classList.remove("filtered-out");
    }
  } else {
    for (let i = 0; i < allRows.length; i++) {
      const row = allRows[i];
      row.classList.remove("filtered-out");
      let shouldHide = false;

      if (!shouldHide && pilotOnlyCharacterIds.size > 0) {
        let anyMatch = false;
        for (const cid of pilotOnlyCharacterIds) {
          if (row.querySelector(`a.pilot-link[data-character-id='${cid}']`)) { anyMatch = true; break; }
        }
        if (!anyMatch) { hiddenByPilot++; shouldHide = true; }
      }

      if (!shouldHide && maxAttackersLimit > 0) {
        const kmRows = row.querySelectorAll(".killmail-row[data-attacker-count]");
        for (let k = 0; k < kmRows.length; k++) {
          const n = parseInt(kmRows[k].getAttribute("data-attacker-count") || "0", 10);
          if (n > maxAttackersLimit) { hiddenByMaxAttackers++; shouldHide = true; break; }
        }
      }

      if (shouldHide) row.classList.add("filtered-out");
    }
  }

  updateFilterIndicator({
    lowsec: hiddenByLowsec,
    nullsec: hiddenByNullsec,
    highsec: hiddenByHighsec,
    pilot: hiddenByPilot,
    tradeHub: hiddenByTradeHub,
    gatecamps: hiddenByGatecamps,
    maxAttackers: hiddenByMaxAttackers
  });

  const showGatecampHints = hideGatecamps && pilotOnlyCharacterIds.size > 0 && gatecampHidSelectedPilotRows;
  updateGatecampFilterHintIconsVisibility(showGatecampHints, Array.from(hiddenSystemsForSelectedPilots));
  saveFilterState();
}

function syncTradeHubCheckboxes() {
  document.querySelectorAll(".trade-hub-checkbox").forEach(cb => {
    const term = (cb.getAttribute("data-trade-hub") || "").toLowerCase();
    if (term) {
      cb.checked = !hiddenTradeHubs.has(term);
    }
  });
}

// Initialize sorting functionality
function initializeSorting() {
  const resultTable = document.getElementById("result");
  if (!resultTable) return;

  const headers = resultTable.querySelectorAll("thead th");
  headers.forEach((header, index) => {
    // Skip Notes column (last column)
    if (index === headers.length - 1) return;
    
    const headerText = header.textContent.trim();
    if (headerText === "Notes" || headerText.includes("Notes")) return;
    
    // Skip if already initialized (sortable class present)
    if (header.classList.contains("sortable")) return;
    
    // Make header sortable
    header.classList.add("sortable");
    
    // Check if this is the Recency column in near_trade_hubs or proximity mode
    const checkedMode = document.querySelector("input[name='mode']:checked");
    const mode = checkedMode ? checkedMode.value : null;
    if (headerText.includes("Recency") && (mode === "near_trade_hubs" || mode === "proximity")) {
      header.classList.add("sort-asc");
    }
    
    header.addEventListener("click", function(e) {
      if (e.target && e.target.closest && e.target.closest("#filter-indicator")) return;
      sortTable(index, header);
    });
  });
}

// Sort table by column index
function sortTable(columnIndex, header) {
  const resultTable = document.getElementById("result");
  if (!resultTable) return;

  const tbody = resultTable.querySelector("tbody");
  if (!tbody) return;

  // Determine sort direction
  const isAscending = !header.classList.contains("sort-asc");
  
  // Remove sort classes from all headers
  const allHeaders = resultTable.querySelectorAll("thead th");
  allHeaders.forEach(h => {
    h.classList.remove("sort-asc", "sort-desc");
  });
  
  // Add sort class to current header
  header.classList.add(isAscending ? "sort-asc" : "sort-desc");
  
  // Sort rows (direct children only — avoid nested attacker-table rows)
  const rows = Array.from(tbody.children);
  rows.sort((a, b) => {
    const aCell = a.cells[columnIndex];
    const bCell = b.cells[columnIndex];
    if (!aCell || !bCell) return 0;
    const aValue = getCellValue(aCell, columnIndex);
    const bValue = getCellValue(bCell, columnIndex);
    if (typeof aValue === "number" && typeof bValue === "number") {
      return isAscending ? aValue - bValue : bValue - aValue;
    }
    const aStr = String(aValue || "").toLowerCase();
    const bStr = String(bValue || "").toLowerCase();
    return isAscending ? aStr.localeCompare(bStr) : bStr.localeCompare(aStr);
  });
  
  // Batch-reorder rows in a single DOM mutation
  const fragment = document.createDocumentFragment();
  for (let i = 0; i < rows.length; i++) fragment.appendChild(rows[i]);
  tbody.replaceChildren(fragment);
  
  // Re-apply security filters only when filters are active
  if (hasActiveFilters()) applySecurityFilters();
}

function hasActiveFilters() {
  const fl = document.getElementById("filter-lowsec");
  const fn = document.getElementById("filter-nullsec");
  const fg = document.getElementById("filter-gatecamps");
  if (fl && !fl.checked) return true;
  if (fn && !fn.checked) return true;
  if (fg && fg.checked) return true;
  return maxAttackersLimit > 0 ||
         pilotOnlyCharacterIds.size > 0 ||
         hiddenTradeHubs.size > 0;
}

// Extract sortable value from cell
function getCellValue(cell, columnIndex) {
  const cache = getCellValue._cache || (getCellValue._cache = {});
  if (!cache.headers) {
    const t = document.getElementById("result");
    cache.headers = t ? Array.from(t.querySelectorAll("thead th")) : [];
  }
  if (columnIndex >= cache.headers.length) return cell.textContent.trim();
  const headerText = cache.headers[columnIndex].textContent.trim();

  if (headerText.startsWith("Range")) {
    const dv = cell.querySelector(".distance-value");
    const t = dv ? dv.textContent : cell.textContent;
    const match = t.match(/(\d+)/);
    return match ? parseInt(match[1], 10) : 0;
  }

  if (headerText.includes("Recency")) {
    const attr = cell.getAttribute("data-recency");
    if (attr) { const v = parseInt(attr, 10); if (!isNaN(v)) return v; }
    const match = cell.textContent.match(/(\d+)/);
    return match ? parseInt(match[1], 10) : 0;
  }

  const link = cell.querySelector("a");
  return link ? link.textContent.trim() : cell.textContent.trim();
}

// Bind jump clone station destination buttons to handle clicks
function bindJumpCloneDestinationButtons() {
  const buttons = document.querySelectorAll(".jump-clone-destination-btn");
  buttons.forEach(button => {
    button.addEventListener("click", async function(e) {
      e.preventDefault();
      e.stopPropagation();

      // Keep success result visible until user clicks to clear it.
      if (button.getAttribute("data-success") === "true") {
        clearDestinationSuccessState(button);
        return;
      }
      
      const systemID = parseInt(button.getAttribute("data-system-id"));
      const systemName = button.getAttribute("data-system-name");
      const stationIDAttr = button.getAttribute("data-station-id");
      const stationID = stationIDAttr ? parseInt(stationIDAttr) : null;
      
      if (!systemID) {
        console.error("CLIENT: Invalid system ID for jump clone destination");
        return;
      }
      
      debugLog("CLIENT: Setting jump clone destination - system:", systemName, "systemID:", systemID, "stationID:", stationID);
      
      // Disable button during request
      button.disabled = true;
      button.textContent = "Setting...";
      
      try {
        // Set destination to the jump clone station (always clear waypoints first)
        // Include station_id if available to set destination exactly to station
        const waypointBody = { system_id: systemID, is_first: true };
        if (stationID && stationID > 0) {
          waypointBody.station_id = stationID;
        }
        const response = await fetch(Config.endpoints.authWaypoint, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
          },
          credentials: "include",
          body: JSON.stringify(waypointBody),
        });
        
        if (!response.ok) {
          if (response.status === 401) {
            const errorText = await response.text();
            showToast(errorText || "Please log in to set waypoints.");
            return;
          }
          if (response.status === 409) {
            const errorText = await response.text();
            showToast(errorText || "Character is offline. Log into EVE Online and try again.");
            clearDestinationSuccessState(button);
            return;
          }
          const errorText = await response.text();
          throw new Error(`Failed to set waypoint: ${response.status} - ${errorText}`);
        }
        
        const data = await response.json();
        if (data.success) {
          clearAllDestinationSuccessStates(button);
          // Show success feedback
          button.textContent = "✓ Set!";
          button.style.backgroundColor = "#4CAF50";
          button.style.color = "white";
          button.disabled = false;
          button.setAttribute("data-success", "true");
        } else {
          throw new Error("Failed to set waypoint");
        }
      } catch (error) {
        console.error("CLIENT: Error setting jump clone destination:", error);
        showToast(`Failed to set destination to ${systemName}: ${error.message}`);
        clearDestinationSuccessState(button);
      }
    });
  });
}

// Bind waypoint buttons to handle clicks
function bindWaypointButtons() {
  const buttons = document.querySelectorAll(".set-destination-btn");
  buttons.forEach(button => {
    button.addEventListener("click", async function(e) {
      e.preventDefault();
      e.stopPropagation();

      // Keep success result visible until user clicks to clear it.
      if (button.getAttribute("data-success") === "true") {
        clearDestinationSuccessState(button);
        return;
      }
      
      const systemID = parseInt(button.getAttribute("data-system-id"));
      const systemName = button.getAttribute("data-system-name");
      const stationIDAttr = button.getAttribute("data-station-id");
      const stationID = stationIDAttr ? parseInt(stationIDAttr) : null;
      const viaThera = button.getAttribute("data-via-thera") === "true";
      const routePathStr = button.getAttribute("data-route-path");
      
      if (!systemID) {
        console.error("CLIENT: Invalid system ID for waypoint");
        return;
      }
      
      // Disable button during request
      button.disabled = true;
      button.textContent = "Setting...";

      // Track whether we've already cleared waypoints in this click flow
      let firstWaypointCleared = false;

      try {
        // If route goes through Thera, set destination twice:
        // 1. First to the system with inbound Thera signature (system right before Thera in path)
        // 2. Then to the target system
        if (viaThera && routePathStr) {
          const routePath = JSON.parse(routePathStr);
          const TheraSystemID = 31000005; // Thera system ID
          
          // Find the system right before Thera in the route path
          let theraInboundSystemID = null;
          for (let i = 0; i < routePath.length - 1; i++) {
            if (routePath[i + 1] === TheraSystemID) {
              theraInboundSystemID = routePath[i];
              break;
            }
          }
          
          if (theraInboundSystemID && theraInboundSystemID !== systemID) {
            // First, set destination to the system with inbound Thera signature
            // Mark this as the first waypoint so backend can clear other waypoints if needed
            // Note: For Thera inbound system, we don't have station_id, so use system_id
            const firstResponse = await fetch(Config.endpoints.authWaypoint, {
              method: "POST",
              headers: {
                "Content-Type": "application/json",
              },
              credentials: "include",
              body: JSON.stringify({ system_id: theraInboundSystemID, is_first: true }),
            });
            
            if (!firstResponse.ok) {
              if (firstResponse.status === 401) {
                const errorText = await firstResponse.text();
                showToast(errorText || "Please log in to set waypoints.");
                return;
              }
              if (firstResponse.status === 409) {
                const errorText = await firstResponse.text();
                showToast(errorText || "Character is offline. Log into EVE Online and try again.");
                clearDestinationSuccessState(button);
                return;
              }
              const errorText = await firstResponse.text();
              throw new Error(`Failed to set first waypoint: ${firstResponse.status} - ${errorText}`);
            }

            // We cleared waypoints when setting this first leg
            firstWaypointCleared = true;
            
            // Wait a bit before setting the second destination
            await new Promise(resolve => setTimeout(resolve, 500));
          }
        }
        
        // Set destination to the target system.
        // If we already set a first waypoint in this flow (Thera inbound), don't clear again.
        // Otherwise (single-destination click or routes without a separate first leg), clear existing waypoints first.
        // Include station_id if available to set destination exactly to station
        const waypointBody = { system_id: systemID, is_first: !firstWaypointCleared };
        if (stationID) {
          waypointBody.station_id = stationID;
        }
        const response = await fetch(Config.endpoints.authWaypoint, {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
          },
          credentials: "include",
          body: JSON.stringify(waypointBody),
        });
        
        if (!response.ok) {
          if (response.status === 401) {
            const errorText = await response.text();
            showToast(errorText || "Please log in to set waypoints.");
            return;
          }
          if (response.status === 409) {
            const errorText = await response.text();
            showToast(errorText || "Character is offline. Log into EVE Online and try again.");
            clearDestinationSuccessState(button);
            return;
          }
          const errorText = await response.text();
          throw new Error(`Failed to set waypoint: ${response.status} - ${errorText}`);
        }
        
        const data = await response.json();
        if (data.success) {
          clearAllDestinationSuccessStates(button);
          // Show success feedback
          button.textContent = "✓ Set!";
          button.style.backgroundColor = "#4CAF50";
          button.style.color = "white";
          button.disabled = false;
          button.setAttribute("data-success", "true");
        } else {
          throw new Error("Failed to set waypoint");
        }
      } catch (error) {
        console.error("CLIENT: Error setting waypoint:", error);
        showToast(`Failed to set destination to ${systemName}: ${error.message}`);
        clearDestinationSuccessState(button);
      }
    });
  });
}

// Show "Set destination" buttons when user is authenticated
function showSetDestinationButtons() {
  const buttons = document.querySelectorAll(".set-destination-btn");
  buttons.forEach(button => {
    // Use "block" so the button always appears below the system name (not beside it)
    button.style.display = "block";
  });
  // Also show jump clone station destination buttons
  const jumpCloneButtons = document.querySelectorAll(".jump-clone-destination-btn");
  jumpCloneButtons.forEach(button => {
    button.style.display = "inline-block";
  });
}

// Hide "Set destination" buttons when user is not authenticated
function hideSetDestinationButtons() {
  const buttons = document.querySelectorAll(".set-destination-btn");
  buttons.forEach(button => {
    button.style.display = "none";
  });
  // Also hide jump clone station destination buttons
  const jumpCloneButtons = document.querySelectorAll(".jump-clone-destination-btn");
  jumpCloneButtons.forEach(button => {
    button.style.display = "none";
  });
}
