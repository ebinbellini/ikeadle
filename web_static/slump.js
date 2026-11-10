/* ---------------------------------------------------------------- *
 * Jag har tur                                                     *
 * A random product to guess the price of. The product id is put    *
 * into the URL, so a reload keeps playing the same item and the    *
 * link can be shared. "Spela igen" simply loads /slump again,      *
 * which draws a new product.                                        *
 * ---------------------------------------------------------------- */

const SLUMP_STORAGE_PREFIX = "ikeadle:slump:";

function slump_config(id) {
    return {
        storage_key: SLUMP_STORAGE_PREFIX + id,
        guess_url: "/api/slump/guess",
        guess_body: (guess) => ({ id: id, guess: guess }),
        answer_url: "/api/slump/answer?id=" + encodeURIComponent(id),
        notes: {
            win: "Snyggt gissat! Dra en ny vara när du vill.",
            lose: "Bara att dra en ny vara och försöka igen.",
        },
        restart: { href: "/slump", label: "Spela igen" },
    };
}

// Puts the product in the URL so a reload does not swap the product.
function remember_product(id) {
    const url = new URL(window.location.href);
    if (url.searchParams.get("id") === id) {
        return;
    }
    url.searchParams.set("id", id);
    window.history.replaceState(null, "", url);
}

// Used when the page was rendered before the scraper had a product.
async function load_product() {
    try {
        const response = await fetch("/api/slump/product", { headers: { Accept: "application/json" } });
        if (!response.ok) {
            throw new Error("HTTP " + response.status);
        }
        const data = await response.json();
        fill_product_card(data.product);
        remember_product(data.id);
        start_game(slump_config(data.id));
    } catch (error) {
        set_status("Kunde inte dra någon vara. Försöker igen…");
        setTimeout(load_product, 5000);
    }
}

document.addEventListener("DOMContentLoaded", () => {
    init_game_ui();

    const root = document.getElementById("game");
    if (!root) {
        return;
    }

    const id = root.dataset.productId || "";
    if (!id) {
        load_product();
        return;
    }

    remember_product(id);
    start_game(slump_config(id));
});
