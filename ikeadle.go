package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ebinbellini/ikeadle/scraper"
	"github.com/ebinbellini/ikeadle/server"
	"github.com/joho/godotenv"
	"github.com/robfig/cron/v3"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using environment variables")
	}

	addr := strings.TrimSpace(os.Getenv("ADDR"))
	if addr == "" {
		log.Fatal("ADDR is required. Set it in the .env file or as an environment variable.")
	}
	os.Setenv("ADDR", addr)

	go server.Run()

	fmt.Println("Loading the IKEA catalogue")
	go func() {
		scraper.LoadOrScrape()
		// Pick today's product as soon as the first products are available.
		if err := server.EnsureDailyProduct(); err != nil {
			fmt.Println("Could not pick daily product:", err)
		}
	}()

	fmt.Println("Activate CRON schedule for scraper")
	sched := strings.TrimSpace(os.Getenv("SCRAPE_CRON_SCHED"))
	if sched == "" {
		log.Fatal("SCRAPE_CRON_SCHED is required. Set it in the .env file or as an environment variable.")
	}
	fmt.Println("Scheduled scrapes run at", sched)
	c := cron.New()
	_, err := c.AddFunc(sched, func() {
		fmt.Println("Running scheduled scraper")
		if err := scraper.Scrape(); err != nil {
			// A failed scrape should not take the game server down. The next
			// scheduled run can try again.
			fmt.Println("Scheduled scrape failed:", err)
		}
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Activate CRON schedule for daily product")
	_, err = c.AddFunc("0 0 * * *", func() {
		fmt.Println("Picking a new daily product")
		if err := server.PickDailyProduct(); err != nil {
			fmt.Println("Could not pick daily product:", err)
		}
	})
	if err != nil {
		log.Fatal(err)
	}

	c.Start()

	select {}
}
