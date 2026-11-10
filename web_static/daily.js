/* ---------------------------------------------------------------- *
 * Dagens vara                                                     *
 * Product of the day: one product for everyone, guesses are saved   *
 * per date.                                                         *
 * ---------------------------------------------------------------- */

const DAILY_STORAGE_PREFIX = "ikeadle:daily:";

function daily_config(date) {
    return {
        storage_key: DAILY_STORAGE_PREFIX + date,
        guess_url: "/api/daily/guess",
        guess_body: (guess) => ({ guess: guess }),
        answer_url: "/api/daily/answer",
        notes: {
            win: "Snyggt gissat! En ny vara väntar vid midnatt.",
            lose: "En ny vara väntar vid midnatt. Då tar du den!",
        },
        restart: null,
    };
}

// Used when the page was rendered before the scraper had a product, so the
// card has to be filled in afterwards.
async function load_daily() {
    try {
        const response = await fetch("/api/daily", { headers: { Accept: "application/json" } });
        if (!response.ok) {
            throw new Error("HTTP " + response.status);
        }
        const data = await response.json();
        fill_product_card(data.product);
        start_game(daily_config(data.date));
    } catch (error) {
        set_status("Kunde inte hämta dagens vara. Försöker igen…");
        setTimeout(load_daily, 5000);
    }
}

document.addEventListener("DOMContentLoaded", () => {
    init_game_ui();

    const root = document.getElementById("game");
    if (!root) {
        return;
    }

    const date = root.dataset.date || "";
    if (!date) {
        load_daily();
        return;
    }

    start_game(daily_config(date));
});
