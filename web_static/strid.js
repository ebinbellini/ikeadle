/* ---------------------------------------------------------------- *
 * Kund mot kund                                                    *
 * En lobby med 2-5 spelare, fem rundor och tio sekunder per          *
 * gissning. Servern äger all speltid och skickar hela spelstatusen   *
 * som server-sent events; den här filen ritar bara det den får.     *
 * ---------------------------------------------------------------- */

const STRID_STORAGE_PREFIX = "ikeadle:strid:";

const strid = {
    code: "",
    codeState: "",
    token: "",
    state: null,
    source: null,
    clockOffset: 0,
    timerHandle: null,
    fallbackHandle: null,
    lastEventAt: 0,
    lastRevision: null,
    lastRoundNumber: 0,
    lastGuess: null,
    viewKey: "",
    busy: false,
    nameFormShown: false,
};

const sEls = {};

/* ---------------------------- Boot ------------------------------- */

document.addEventListener("DOMContentLoaded", () => {
    cache_strid_elements();
    bind_strid_events();

    const root = document.getElementById("strid");
    if (!root) {
        return;
    }

    strid.code = (root.dataset.code || "").toUpperCase();
    strid.codeState = root.dataset.codeState || "";
    strid.token = storage_get(strid_key(strid.code));

    if (!strid.code) {
        show_section("landing");
        return;
    }

    sEls.joinCode.textContent = strid.code;

    if (strid.token) {
        connect_strid();
        return;
    }

    show_code_entry(strid.codeState);
});

function cache_strid_elements() {
    const ids = {
        error: "strid-error",
        connecting: "strid-connecting",
        landing: "strid-landing",
        join: "strid-join",
        lobby: "strid-lobby",
        match: "strid-match",
        finished: "strid-finished",
        message: "strid-message",

        create: "strid-create",
        codeForm: "strid-code-form",
        codeInput: "strid-code-input",

        joinCode: "strid-join-code",
        joinForm: "strid-join-form",
        joinName: "strid-join-name",

        code: "strid-code",
        copyCode: "strid-copy-code",
        copyLink: "strid-copy-link",
        copyStatus: "strid-copy-status",

        nameForm: "strid-name-form",
        nameInput: "strid-name-input",

        playerCount: "strid-player-count",
        players: "strid-players",
        lobbyNote: "strid-lobby-note",
        start: "strid-start",
        waiting: "strid-waiting",

        roundLabel: "strid-round-label",
        timer: "strid-timer",
        standings: "strid-standings",

        card: "product-card",
        image: "product-image",
        name: "product-name",
        category: "product-category",
        description: "product-description",
        amount: "product-amount",

        guessForm: "strid-guess-form",
        guessInput: "strid-guess-input",
        guessSubmit: "strid-guess-submit",
        guessError: "strid-guess-error",
        guessLocked: "strid-guess-locked",

        roundResult: "strid-round-result",
        roundResultTitle: "strid-round-result-title",
        roundPrice: "strid-round-price",
        roundClosest: "strid-round-closest",
        roundResults: "strid-round-results",

        winnerTitle: "strid-winner-title",
        winnerNames: "strid-winner-names",
        finalStandings: "strid-final-standings",
        again: "strid-again",
        recapCard: "strid-recap-card",
        recap: "strid-recap",
        recapHint: "strid-recap-hint",

        messageTitle: "strid-message-title",
        messageBody: "strid-message-body",
    };

    Object.entries(ids).forEach(([key, id]) => {
        sEls[key] = document.getElementById(id);
    });
}

function bind_strid_events() {
    if (!sEls.create) {
        return;
    }

    sEls.create.addEventListener("click", create_lobby);
    sEls.codeForm.addEventListener("submit", open_invite);
    sEls.joinForm.addEventListener("submit", join_lobby);
    sEls.nameForm.addEventListener("submit", submit_name);
    sEls.start.addEventListener("click", start_match);
    if (sEls.again) {
        sEls.again.addEventListener("click", play_again);
    }
    sEls.guessForm.addEventListener("submit", submit_guess);

    sEls.codeInput.addEventListener("input", () => {
        const cleaned = sEls.codeInput.value.toUpperCase().replace(/[^A-Z0-9]/g, "").slice(0, 4);
        if (sEls.codeInput.value !== cleaned) {
            sEls.codeInput.value = cleaned;
        }
    });

    sEls.copyCode.addEventListener("click", () => copy_value(strid.code, "Koden kopierad!"));
    sEls.copyLink.addEventListener("click", () => {
        const link = window.location.origin + "/strid/" + strid.code;
        copy_value(link, "Inbjudningslänken kopierad!");
    });

    document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "visible" && strid.token) {
            refresh_state();
        }
    });
}

/* --------------------------- Navigation -------------------------- */

const SECTIONS = ["connecting", "landing", "join", "lobby", "match", "finished", "message"];

function show_section(name) {
    SECTIONS.forEach((section) => {
        if (sEls[section]) {
            sEls[section].hidden = section !== name;
        }
    });
}

function open_invite(event) {
    event.preventDefault();
    clear_error();

    const code = sEls.codeInput.value.trim().toUpperCase();
    if (!/^[A-Z0-9]{4}$/.test(code)) {
        show_error("Koden ska vara fyra tecken (A-Z eller 0-9).");
        return;
    }

    window.location.href = "/strid/" + code;
}

function show_code_entry(codeState) {
    const info = {
        missing: [
            "Lobbyn finns inte",
            "Koden stämmer inte, eller så har lobbyn stängts. Be värden om en ny kod.",
        ],
        started: [
            "Matchen har redan startat",
            "Du kan inte hoppa in i en match som är igång. Be värden skapa en ny lobby.",
        ],
        finished: [
            "Matchen är redan avgjord",
            "Den här lobbyn har spelat klart. Skapa en ny lobby för en ny match.",
        ],
        full: [
            "Lobbyn är full",
            "Det får plats högst fem spelare i en lobby. Be värden skapa en ny.",
        ],
    };

    if (!codeState || codeState === "open") {
        show_section("join");
        setTimeout(() => sEls.joinName.focus(), 0);
        return;
    }

    const message = info[codeState];
    if (message) {
        show_message(message[0], message[1]);
        return;
    }

    show_section("join");
}

function show_message(title, body) {
    sEls.messageTitle.textContent = title;
    sEls.messageBody.textContent = body;
    show_section("message");
}

/* ---------------------------- Connection ------------------------- */

async function connect_strid() {
    show_section("connecting");

    try {
        const state = await fetch_state();
        strid.lastEventAt = Date.now();
        apply_state(state);
        open_events();
        start_fallback();
    } catch (error) {
        if (error.status === 404) {
            forget_token();
            show_code_entry("missing");
            return;
        }
        if (error.status === 401) {
            forget_token();
            show_code_entry(strid.codeState);
            return;
        }
        show_message("Kunde inte ansluta", "Servern svarade inte. Ladda om sidan och försök igen.");
    }
}

function open_events() {
    close_events();

    const url = "/api/strid/events?code=" + encodeURIComponent(strid.code) +
        "&token=" + encodeURIComponent(strid.token);
    const source = new EventSource(url);
    strid.source = source;

    source.addEventListener("state", (event) => {
        strid.lastEventAt = Date.now();
        try {
            apply_state(JSON.parse(event.data));
        } catch (error) {
            // A broken payload is ignored; the next state or the fallback
            // refresh repairs the view.
        }
    });

    source.onopen = () => {
        strid.lastEventAt = Date.now();
    };

    source.onerror = () => {
        if (source.readyState === EventSource.CLOSED && strid.source === source) {
            setTimeout(() => {
                if (strid.token && strid.source === source) {
                    open_events();
                }
            }, 3000);
        }
    };
}

function close_events() {
    if (strid.source) {
        strid.source.close();
        strid.source = null;
    }
}

function start_fallback() {
    if (strid.fallbackHandle) {
        return;
    }

    strid.fallbackHandle = setInterval(async () => {
        if (!strid.token) {
            return;
        }
        const open = strid.source && strid.source.readyState === EventSource.OPEN;
        if (open && Date.now() - strid.lastEventAt < 25000) {
            return;
        }
        await refresh_state();
    }, 5000);
}

async function fetch_state() {
    const url = "/api/strid/state?code=" + encodeURIComponent(strid.code) +
        "&token=" + encodeURIComponent(strid.token);
    const response = await fetch(url, { headers: { Accept: "application/json" } });
    const data = await response.json().catch(() => ({}));

    if (!response.ok) {
        const error = new Error(data.error || "Kunde inte hämta lobbyn.");
        error.status = response.status;
        throw error;
    }
    return data;
}

async function refresh_state() {
    try {
        const state = await fetch_state();
        strid.lastEventAt = Date.now();
        apply_state(state);
    } catch (error) {
        if (error.status === 404) {
            forget_token();
            show_code_entry("missing");
        } else if (error.status === 401) {
            forget_token();
            show_code_entry(strid.codeState);
        }
    }
}

function render_current_view() {
    if (!strid.state) {
        return;
    }
    if (strid.state.status === "waiting") {
        render_lobby();
    } else if (strid.state.status === "playing") {
        render_match(false);
    } else {
        render_finished();
    }
}

function apply_state(state) {
    // A state fetch can finish after a newer event stream update. The server
    // revision makes sure an older snapshot never overwrites a newer one.
    if (typeof state.revision === "number") {
        if (strid.lastRevision !== null && state.revision < strid.lastRevision) {
            return;
        }
        strid.lastRevision = state.revision;
    }

    strid.state = state;
    // The server owns the clock. Remember the offset so every countdown in the
    // browser runs on the server's time rather than the device's.
    strid.clockOffset = state.server_now - Date.now();

    const roundNumber = state.round ? state.round.number : 0;
    const viewKey = state.status + ":" + state.phase + ":" + roundNumber;
    const changed = viewKey !== strid.viewKey;
    strid.viewKey = viewKey;

    if (state.status === "waiting") {
        stop_timer();
        render_lobby();
    } else if (state.status === "playing") {
        render_match(changed);
        start_timer();
    } else {
        stop_timer();
        render_finished();
    }

    update_countdown();
}

/* ----------------------------- Lobby ----------------------------- */

async function create_lobby() {
    if (strid.busy) {
        return;
    }

    clear_error();
    strid.busy = true;
    sEls.create.disabled = true;

    try {
        const data = await api_post("/api/strid/lobbies", {});
        strid.code = data.code;
        strid.token = data.token;
        storage_set(strid_key(strid.code), data.token);
        window.history.replaceState(null, "", "/strid/" + data.code);
        // The lobby is usable as soon as the create request returns. Do not
        // keep the busy flag set during the initial state fetch, or a quick
        // Redo press in the name form would be ignored.
        strid.busy = false;
        await connect_strid();
    } catch (error) {
        show_error(error.message);
    } finally {
        strid.busy = false;
        sEls.create.disabled = false;
    }
}

async function join_lobby(event) {
    event.preventDefault();
    if (strid.busy) {
        return;
    }

    clear_error();
    const name = sEls.joinName.value.trim();
    if (!name) {
        show_error("Ange ett användarnamn.");
        return;
    }

    strid.busy = true;
    const button = sEls.joinForm.querySelector("button");
    button.disabled = true;

    try {
        const data = await api_post("/api/strid/join", { code: strid.code, name: name });
        strid.token = data.token;
        storage_set(strid_key(strid.code), data.token);
        strid.busy = false;
        await connect_strid();
    } catch (error) {
        show_error(error.message);
    } finally {
        strid.busy = false;
        button.disabled = false;
    }
}

function render_lobby() {
    show_section("lobby");

    const state = strid.state;
    const you = find_you();

    sEls.code.textContent = state.code;
    sEls.joinCode.textContent = state.code;
    sEls.playerCount.textContent = state.players.length + " / " + state.max_players;

    render_player_list();

    const showNameForm = Boolean(you && you.host && !you.ready);
    sEls.nameForm.hidden = !showNameForm;
    if (showNameForm && !strid.nameFormShown) {
        setTimeout(() => sEls.nameInput.focus(), 0);
    }
    strid.nameFormShown = showNameForm;

    const isHost = Boolean(you && you.host);
    sEls.start.hidden = !isHost;
    sEls.start.disabled = !state.can_start || strid.busy;
    sEls.waiting.hidden = isHost || !you;

    sEls.lobbyNote.textContent = lobby_note(state, you);
}

function render_player_list() {
    sEls.players.textContent = "";

    strid.state.players.forEach((player) => {
        const item = document.createElement("li");
        item.className = "strid-player" + (player.you ? " you" : "");

        const name = document.createElement("span");
        name.className = "strid-player-name";
        name.textContent = player.name || (player.host ? "Värden" : "Spelare");

        const badges = document.createElement("span");
        badges.className = "strid-badges";

        if (player.host) {
            badges.appendChild(badge("Värd", "host"));
        }
        if (player.you) {
            badges.appendChild(badge("Du", "you"));
        }
        if (player.ready) {
            badges.appendChild(badge("Redo", "ready"));
        }
        if (!player.connected) {
            badges.appendChild(badge("Offline", "offline"));
        }

        item.append(name, badges);
        sEls.players.appendChild(item);
    });
}

function badge(label, kind) {
    const element = document.createElement("span");
    element.className = "strid-badge " + kind;
    element.textContent = label;
    return element;
}

function lobby_note(state, you) {
    const missing = state.min_players - state.players.length;
    if (missing > 0) {
        const players = missing === 1 ? "spelare" : "spelare";
        return "Minst " + state.min_players + " spelare behövs. Dela koden så att " +
            missing + " " + players + " till kan gå med.";
    }

    if (!you) {
        return "";
    }
    if (!you.ready) {
        return "Välj ett användarnamn och tryck på Redo för att kunna starta.";
    }
    if (!state.can_start) {
        return "Alla måste vara redo innan matchen kan starta.";
    }
    if (you.host) {
        return "Alla är redo - du kan starta matchen.";
    }
    return "Alla är redo. Väntar på att värden startar matchen…";
}

async function submit_name(event) {
    event.preventDefault();
    if (strid.busy) {
        return;
    }

    clear_error();
    const name = sEls.nameInput.value.trim();
    if (!name) {
        show_error("Ange ett användarnamn.");
        return;
    }

    strid.busy = true;
    const button = sEls.nameForm.querySelector("button");
    button.disabled = true;

    try {
        await api_post("/api/strid/ready", {
            code: strid.code,
            token: strid.token,
            name: name,
        });
        sEls.nameInput.value = "";
        await refresh_state();
    } catch (error) {
        show_error(error.message);
    } finally {
        strid.busy = false;
        button.disabled = false;
        if (strid.state) {
            render_lobby();
        }
    }
}

async function start_match() {
    if (strid.busy) {
        return;
    }

    clear_error();
    strid.busy = true;
    sEls.start.disabled = true;

    try {
        await api_post("/api/strid/start", { code: strid.code, token: strid.token });
        await refresh_state();
    } catch (error) {
        show_error(error.message);
    } finally {
        strid.busy = false;
        render_current_view();
    }
}

/* ----------------------------- Match ----------------------------- */

function render_match(changed) {
    show_section("match");

    const state = strid.state;
    const round = state.round;
    const you = find_you();
    if (!round) {
        return;
    }

    if (round.number !== strid.lastRoundNumber) {
        strid.lastRoundNumber = round.number;
        strid.lastGuess = null;
        clear_guess_error();
        sEls.guessInput.value = "";
    }

    sEls.roundLabel.textContent = "Runda " + round.number + " av " + round.total;
    fill_product(round.product);
    render_standings();

    const guessing = state.phase === "guessing";
    const guessed = Boolean(you && you.guessed);
    const showForm = guessing && !guessed && !strid.busy;

    sEls.guessForm.hidden = !showForm;
    sEls.guessLocked.hidden = !(guessing && guessed);
    sEls.roundResult.hidden = !(state.phase === "reveal");

    if (guessing && guessed) {
        const value = you.guess !== undefined && you.guess !== null ? you.guess : strid.lastGuess;
        sEls.guessLocked.textContent = value !== null && value !== undefined
            ? "Du gissade " + format_kr(value) + ". Väntar på att andra spelare ska gissa…"
            : "Du har gissat. Väntar på att andra spelare ska gissa…";
    }

    sEls.guessInput.disabled = !showForm;
    sEls.guessSubmit.disabled = !showForm;
    sEls.guessSubmit.textContent = strid.busy ? "Gissar…" : "Gissa";

    if (state.phase === "reveal") {
        render_round_result(round);
    }

    if (changed && showForm) {
        setTimeout(() => sEls.guessInput.focus(), 0);
    }
}

function fill_product(product) {
    sEls.card.hidden = false;
    sEls.image.src = product.image_url || "";
    sEls.image.alt = product.name || "Varan";
    sEls.name.textContent = product.name || "";
    sEls.category.textContent = product.category || "";
    sEls.category.hidden = !product.category;
    sEls.description.textContent = product.description || "";
    sEls.description.hidden = !product.description;
    sEls.amount.textContent = product.amount || "";
    sEls.amount.hidden = !product.amount;
}

function render_standings() {
    sEls.standings.textContent = "";

    const players = [...strid.state.players].sort((a, b) => {
        if (b.score !== a.score) {
            return b.score - a.score;
        }
        return (a.name || "").localeCompare(b.name || "", "sv");
    });

    players.forEach((player) => {
        const item = document.createElement("div");
        item.className = "strid-standing" + (player.you ? " you" : "");

        const place = document.createElement("span");
        place.className = "strid-standing-place";
        place.textContent = player.place ? "#" + player.place : "#-";

        const name = document.createElement("span");
        name.className = "strid-standing-name";
        name.textContent = player.name || "Spelare";

        const score = document.createElement("span");
        score.className = "strid-standing-score";
        score.textContent = format_points(player.score);

        item.append(place, name, score);
        sEls.standings.appendChild(item);
    });
}

function render_round_result(round) {
    sEls.roundResultTitle.textContent = round.number >= round.total
        ? "Sista rundan är slut"
        : "Rundan är slut";

    sEls.roundPrice.textContent = round.price !== undefined && round.price !== null
        ? "Priset var " + format_kr(round.price)
        : "";

    const entries = [...strid.state.players];
    entries.sort(compare_results);

    const closest = entries.filter((player) => player.closest && player.name);
    sEls.roundClosest.textContent = closest.length
        ? "Närmast: " + closest.map((player) => player.name).join(" & ")
        : "Ingen hann gissa den här rundan.";

    sEls.roundResults.textContent = "";
    entries.forEach((player) => {
        const item = document.createElement("li");
        item.className = "strid-result" +
            (player.closest ? " closest" : "") +
            (player.you ? " you" : "");

        const name = document.createElement("span");
        name.className = "strid-result-name";
        name.textContent = (player.name || "Spelare") + (player.you ? " (du)" : "");
        if (player.closest) {
            name.appendChild(badge(player.exact ? "Exakt" : "Närmast", "closest"));
        }

        const guess = document.createElement("span");
        guess.className = "strid-result-guess";
        guess.textContent = has_number(player.guess) ? format_kr(player.guess) : "Ingen gissning";

        const distance = document.createElement("span");
        distance.className = "strid-result-distance";
        if (!has_number(player.guess)) {
            distance.textContent = "";
        } else if (player.exact) {
            distance.textContent = "Exakt";
        } else {
            distance.textContent = format_kr(player.distance) + " ifrån";
        }

        const points = document.createElement("span");
        points.className = "strid-result-points";
        points.textContent = has_number(player.points) ? "+" + format_points(player.points) : "";

        item.append(name, guess, distance, points);
        sEls.roundResults.appendChild(item);
    });
}

function compare_results(a, b) {
    const aHas = has_number(a.guess);
    const bHas = has_number(b.guess);
    if (aHas !== bHas) {
        return aHas ? -1 : 1;
    }
    if (aHas && bHas && a.distance !== b.distance) {
        return a.distance - b.distance;
    }
    return (a.name || "").localeCompare(b.name || "", "sv");
}

async function submit_guess(event) {
    event.preventDefault();
    if (strid.busy || !strid.state || strid.state.phase !== "guessing") {
        return;
    }

    const guess = parse_guess(sEls.guessInput.value);
    if (guess === null) {
        show_guess_error("Ange ett pris i kronor, till exempel 24,50.");
        return;
    }
    clear_guess_error();

    strid.busy = true;
    sEls.guessInput.disabled = true;
    sEls.guessSubmit.disabled = true;
    sEls.guessSubmit.textContent = "Gissar…";

    try {
        const data = await api_post("/api/strid/guess", {
            code: strid.code,
            token: strid.token,
            guess: guess,
        });
        strid.lastGuess = data.guess;
        const you = find_you();
        if (you) {
            you.guessed = true;
            you.guess = data.guess;
        }
        sEls.guessInput.value = "";
        await refresh_state();
    } catch (error) {
        show_guess_error(error.message);
    } finally {
        strid.busy = false;
        if (strid.state && strid.state.status === "playing") {
            render_match(false);
        }
    }
}

/* ---------------------------- Finished --------------------------- */

function render_finished() {
    show_section("finished");

    const state = strid.state;
    const winners = state.winners || [];

    sEls.winnerTitle.textContent = winners.length > 1 ? "Delad seger" : "Vinnare";
    sEls.winnerNames.textContent = winners.length ? winners.join(" & ") : "Ingen vinnare";

    const players = [...state.players].sort((a, b) => {
        if (a.place !== b.place) {
            return (a.place || 99) - (b.place || 99);
        }
        return (a.name || "").localeCompare(b.name || "", "sv");
    });

    sEls.finalStandings.textContent = "";
    players.forEach((player) => {
        const item = document.createElement("li");
        item.className = "strid-final-row" + (player.you ? " you" : "");

        const place = document.createElement("span");
        place.className = "strid-final-place";
        place.textContent = player.place ? player.place : "-";

        const name = document.createElement("span");
        name.className = "strid-final-name";
        name.textContent = (player.name || "Spelare") + (player.you ? " (du)" : "");
        if (player.winner) {
            name.appendChild(badge("Vinnare", "winner"));
        }

        const score = document.createElement("span");
        score.className = "strid-final-score";
        score.textContent = format_points(player.score);

        item.append(place, name, score);
        sEls.finalStandings.appendChild(item);
    });

    if (sEls.again) {
        sEls.again.disabled = strid.busy;
    }

    render_recap(state);
}

async function play_again() {
    if (strid.busy) {
        return;
    }

    clear_error();
    strid.busy = true;
    if (sEls.again) {
        sEls.again.disabled = true;
    }

    try {
        await api_post("/api/strid/again", {
            code: strid.code,
            token: strid.token,
        });
        await refresh_state();
    } catch (error) {
        show_error(error.message);
        await refresh_state();
    } finally {
        strid.busy = false;
        render_current_view();
    }
}

// The recap shows every round's product and every customer's guess in a table.
// Green cells are exact, red cells are the closest guess of that round.
function render_recap(state) {
    if (!sEls.recapCard || !sEls.recap) {
        return;
    }

    const recap = state.recap;
    if (!recap || !Array.isArray(recap.rounds) || recap.rounds.length === 0) {
        sEls.recapCard.hidden = true;
        sEls.recap.textContent = "";
        if (sEls.recapHint) {
            sEls.recapHint.hidden = true;
        }
        return;
    }

    const players = Array.isArray(recap.players) ? recap.players : [];
    const you = find_you();
    const youName = you ? you.name : "";

    sEls.recapCard.hidden = false;
    sEls.recap.textContent = "";

    const table = document.createElement("table");
    table.className = "strid-recap-table";

    const head = document.createElement("thead");
    const headRow = document.createElement("tr");
    ["Runda", "Vara", "Pris", ...players].forEach((label, index) => {
        const cell = document.createElement("th");
        cell.textContent = label;
        if (index === 0) {
            cell.className = "strid-recap-round";
        }
        if (index >= 3 && label === youName) {
            cell.classList.add("you");
        }
        headRow.appendChild(cell);
    });
    head.appendChild(headRow);
    table.appendChild(head);

    const body = document.createElement("tbody");
    recap.rounds.forEach((round) => {
        const row = document.createElement("tr");

        const roundCell = document.createElement("td");
        roundCell.className = "strid-recap-round";
        roundCell.textContent = round.number;
        row.appendChild(roundCell);

        const productCell = document.createElement("td");
        productCell.className = "strid-recap-product";
        productCell.textContent = round.product && round.product.name
            ? round.product.name
            : "Vara";
        const amount = round.product && round.product.amount ? round.product.amount : "";
        if (amount) {
            const meta = document.createElement("span");
            meta.className = "strid-recap-meta";
            meta.textContent = amount;
            productCell.appendChild(meta);
        }
        row.appendChild(productCell);

        const priceCell = document.createElement("td");
        priceCell.className = "strid-recap-price";
        priceCell.textContent = format_kr(round.price);
        row.appendChild(priceCell);

        const guesses = Array.isArray(round.guesses) ? round.guesses : [];
        const points = Array.isArray(round.points) ? round.points : [];
        const best = points.length ? Math.max(...points) : 0;

        players.forEach((name, index) => {
            const cell = document.createElement("td");
            cell.className = "strid-recap-guess";

            const guess = guesses[index];
            if (!has_number(guess)) {
                cell.textContent = "-";
                cell.classList.add("missing");
            } else {
                cell.textContent = format_kr(guess);
                const exact = Math.round(guess * 100) === Math.round(round.price * 100);
                if (exact) {
                    cell.classList.add("exact");
                } else if (best > 0 && points[index] === best) {
                    cell.classList.add("closest");
                }
            }

            if (name === youName) {
                cell.classList.add("you");
            }
            row.appendChild(cell);
        });

        body.appendChild(row);
    });
    table.appendChild(body);
    sEls.recap.appendChild(table);

    if (sEls.recapHint) {
        sEls.recapHint.hidden = sEls.recap.scrollWidth <= sEls.recap.clientWidth + 1;
    }
}

/* ---------------------------- Countdown -------------------------- */

function start_timer() {
    stop_timer();
    update_countdown();
    strid.timerHandle = setInterval(update_countdown, 200);
}

function stop_timer() {
    if (strid.timerHandle) {
        clearInterval(strid.timerHandle);
        strid.timerHandle = null;
    }
}

function update_countdown() {
    const state = strid.state;
    if (!state || state.status !== "playing" || !state.round) {
        return;
    }

    const round = state.round;
    const now = Date.now() + strid.clockOffset;

    if (state.phase === "guessing") {
        const remaining = Math.max(0, round.ends_at - now);
        const seconds = Math.ceil(remaining / 1000);
        sEls.timer.textContent = seconds + " s";
        sEls.timer.classList.toggle("urgent", seconds <= 3);

        if (remaining <= 0 && !sEls.guessForm.hidden) {
            sEls.guessInput.disabled = true;
            sEls.guessSubmit.disabled = true;
            if (sEls.guessError.hidden) {
                show_guess_error("Tiden är ute. Rundan avslutas…");
            }
        }
        return;
    }

    if (state.phase === "reveal") {
        const remaining = Math.max(0, round.reveal_ends_at - now);
        const seconds = Math.ceil(remaining / 1000);
        sEls.timer.classList.remove("urgent");
        sEls.timer.textContent = round.number >= round.total
            ? "Slutresultat: " + seconds + " s"
            : "Nästa runda: " + seconds + " s";
    }
}

/* ----------------------------- Helpers --------------------------- */

async function api_post(url, body) {
    let response;
    try {
        response = await fetch(url, {
            method: "POST",
            headers: { "Content-Type": "application/json", Accept: "application/json" },
            body: JSON.stringify(body),
        });
    } catch (error) {
        throw new Error("Kunde inte nå servern. Kontrollera anslutningen.");
    }

    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
        throw new Error(data.error || "Något gick fel. Försök igen.");
    }
    return data;
}

function find_you() {
    if (!strid.state) {
        return null;
    }
    return strid.state.players.find((player) => player.you) || null;
}

function parse_guess(raw) {
    if (typeof raw !== "string") {
        return null;
    }

    const normalized = raw.trim().replace(/\s/g, "").replace(",", ".");
    if (normalized === "") {
        return null;
    }

    const value = Number(normalized);
    if (!Number.isFinite(value) || value <= 0) {
        return null;
    }

    return Math.round(value * 100) / 100;
}

function has_number(value) {
    return typeof value === "number" && Number.isFinite(value);
}

function format_kr(value) {
    if (!Number.isFinite(value)) {
        return "";
    }
    const whole = Math.abs(value - Math.round(value)) < 0.005;
    return new Intl.NumberFormat("sv-SE", {
        minimumFractionDigits: whole ? 0 : 2,
        maximumFractionDigits: 2,
    }).format(value) + " kr";
}

function format_points(value) {
    if (!Number.isFinite(value)) {
        return "0 p";
    }
    return new Intl.NumberFormat("sv-SE").format(Math.round(value)) + " p";
}

function show_error(message) {
    sEls.error.textContent = message;
    sEls.error.hidden = false;
}

function clear_error() {
    sEls.error.textContent = "";
    sEls.error.hidden = true;
}

function show_guess_error(message) {
    sEls.guessError.textContent = message;
    sEls.guessError.hidden = false;
}

function clear_guess_error() {
    sEls.guessError.textContent = "";
    sEls.guessError.hidden = true;
}

function strid_key(code) {
    return STRID_STORAGE_PREFIX + code;
}

function storage_get(key) {
    try {
        return window.localStorage.getItem(key);
    } catch (error) {
        return null;
    }
}

function storage_set(key, value) {
    try {
        window.localStorage.setItem(key, value);
    } catch (error) {
        // Private mode or a full quota: the lobby still works for this page.
    }
}

function forget_token() {
    strid.token = "";
    stop_timer();
    close_events();
    if (strid.code) {
        try {
            window.localStorage.removeItem(strid_key(strid.code));
        } catch (error) {
            // Nothing to clean up.
        }
    }
}

async function copy_value(value, message) {
    clear_error();
    try {
        await copy_to_clipboard(value);
        sEls.copyStatus.textContent = message;
        sEls.copyStatus.hidden = false;
        setTimeout(() => {
            sEls.copyStatus.hidden = true;
        }, 2500);
    } catch (error) {
        sEls.copyStatus.textContent = "Kunde inte kopiera. Markera texten manuellt.";
        sEls.copyStatus.hidden = false;
    }
}

async function copy_to_clipboard(value) {
    if (navigator.clipboard && window.isSecureContext) {
        try {
            await navigator.clipboard.writeText(value);
            return;
        } catch (error) {
            // Fall through to the textarea trick, which works without
            // clipboard permissions.
        }
    }

    const area = document.createElement("textarea");
    area.value = value;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.appendChild(area);
    area.select();

    const copied = document.execCommand("copy");
    area.remove();
    if (!copied) {
        throw new Error("copy failed");
    }
}
