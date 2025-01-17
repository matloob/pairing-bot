package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"time"
)

const owner string = `@_**Maren Beam (SP2'19)**`
const oddOneOutMessage string = "OK this is awkward.\nThere were an odd number of people in the match-set today, which means that one person couldn't get paired. Unfortunately, it was you -- I'm really sorry :(\nI promise it's not personal, it was very much random. Hopefully this doesn't happen again too soon. Enjoy your day! <3"
const matchedMessage = "Hi you two! You've been matched for pairing :)\n\nHave fun!"
const offboardedMessage = "Hi! You've been unsubscribed from Pairing Bot.\n\nThis happens at the end of every batch, and everyone is offboarded even if they're still in batch. If you'd like to re-subscribe, just send me a message that says `subscribe`.\n\nBe well! :)"

var maintenanceMode = false

// this is the "id" field from zulip, and is a permanent user ID that's not secret
// Pairing Bot's owner can add their ID here for testing. ctrl+f "ownerID" to see where it's used
const ownerID = "215391"

type PairingLogic struct {
	rdb   RecurserDB
	adb   APIAuthDB
	pdb   PairingsDB
	revdb ReviewDB
	ur    userRequest
	un    userNotification
	sm    streamMessage
	rcapi RecurseAPI
}

var randSrc = rand.New(rand.NewSource(time.Now().UnixNano()))

func (pl *PairingLogic) handle(w http.ResponseWriter, r *http.Request) {
	var err error

	responder := json.NewEncoder(w)

	// check and authorize the incoming request
	// observation: we only validate requests for /webhooks, i.e. user input through zulip

	ctx := r.Context()

	log.Println("Handling a new Zulip request")

	if err = pl.ur.validateJSON(r); err != nil {
		log.Println(err)
		http.NotFound(w, r)
	}

	botAuth, err := pl.adb.GetKey(ctx, "botauth", "token")
	if err != nil {
		log.Println("Something weird happened trying to read the auth token from the database")
	}

	if !pl.ur.validateAuthCreds(botAuth) {
		http.NotFound(w, r)
	}

	intro := pl.ur.validateInteractionType()
	if intro != nil {
		if err = responder.Encode(intro); err != nil {
			log.Println(err)
		}
		return
	}

	ignore := pl.ur.ignoreInteractionType()
	if ignore != nil {
		if err = responder.Encode(ignore); err != nil {
			log.Println(err)
		}
		return
	}

	userData := pl.ur.extractUserData()

	// for testing only
	// this responds with a maintenance message and quits if the request is coming from anyone other than the owner
	if maintenanceMode {
		if userData.userID != ownerID {
			if err = responder.Encode(botResponse{`pairing bot is down for maintenance`}); err != nil {
				log.Println(err)
			}
			return
		}
	}

	log.Printf("The user: %s issued the following request to Pairing Bot: %s", userData.userEmail, pl.ur.getCommandString())

	// you *should* be able to throw any string at this thing and get back a valid command for dispatch()
	// if there are no commad arguments, cmdArgs will be nil
	cmd, cmdArgs, err := pl.ur.sanitizeUserInput()
	if err != nil {
		log.Println(err)
	}

	// the tofu and potatoes right here y'all
	response, err := dispatch(ctx, pl, cmd, cmdArgs, userData.userID, userData.userEmail, userData.userName)
	if err != nil {
		log.Println(err)
	}

	if err = responder.Encode(botResponse{response}); err != nil {
		log.Println(err)
	}
}

// "match" makes matches for pairing, and messages those people to notify them of their match
// it runs once per day at 8am (it's triggered with app engine's cron service)
func (pl *PairingLogic) match(w http.ResponseWriter, r *http.Request) {
	// Check that the request is originating from within app engine
	// https://cloud.google.com/appengine/docs/flexible/go/scheduling-jobs-with-cron-yaml#validating_cron_requests
	if r.Header.Get("X-Appengine-Cron") != "true" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	recursersList, err := pl.rdb.ListPairingTomorrow(ctx)
	log.Println(recursersList)
	if err != nil {
		log.Printf("Could not get list of recursers from DB: %s\n", err)
	}

	skippersList, err := pl.rdb.ListSkippingTomorrow(ctx)
	if err != nil {
		log.Printf("Could not get list of skippers from DB: %s\n", err)
	}

	// get everyone who was set to skip today and set them back to isSkippingTomorrow = false
	for _, skipper := range skippersList {
		err := pl.rdb.UnsetSkippingTomorrow(ctx, skipper)
		if err != nil {
			log.Printf("Could not unset skipping for recurser %v: %s\n", skipper.id, err)
		}
	}

	// shuffle our recursers. This will not error if the list is empty
	randSrc.Shuffle(len(recursersList), func(i, j int) { recursersList[i], recursersList[j] = recursersList[j], recursersList[i] })

	// if for some reason there's no matches today, we're done
	if len(recursersList) == 0 {
		log.Println("No one was signed up to pair today -- so there were no matches")
		return
	}

	// message the peeps!
	botPassword, err := pl.adb.GetKey(ctx, "apiauth", "key")
	if err != nil {
		log.Println("Something weird happened trying to read the auth token from the database")
	}

	// if there's an odd number today, message the last person in the list
	// and tell them they don't get a match today, then knock them off the list
	if len(recursersList)%2 != 0 {
		recurser := recursersList[len(recursersList)-1]
		recursersList = recursersList[:len(recursersList)-1]
		log.Println("Someone was the odd-one-out today")

		err := pl.un.sendUserMessage(ctx, botPassword, recurser.email, oddOneOutMessage)
		if err != nil {
			log.Printf("Error when trying to send oddOneOut message to %s: %s\n", recurser.email, err)
		}
	}

	for i := 0; i < len(recursersList); i += 2 {

		emails := recursersList[i].email + ", " + recursersList[i+1].email
		err := pl.un.sendUserMessage(ctx, botPassword, emails, matchedMessage)
		if err != nil {
			log.Printf("Error when trying to send matchedMessage to %s: %s\n", emails, err)
		}
		log.Println(recursersList[i].email, "was", "matched", "with", recursersList[i+1].email)
	}

	numRecursersPairedUp := len(recursersList)

	log.Printf("Pairing Bot paired up %d recursers today", numRecursersPairedUp)

	numPairings := numRecursersPairedUp / 2

	timestamp := time.Now().Unix()
	pl.pdb.SetNumPairings(ctx, int(timestamp), numPairings)
}

//Unsubscribe people from Pairing Bot when their batch is over. They're always welcome to re-subscribe manually!
func (pl *PairingLogic) endofbatch(w http.ResponseWriter, r *http.Request) {
	// Check that the request is originating from within app engine
	// https://cloud.google.com/appengine/docs/flexible/go/scheduling-jobs-with-cron-yaml#validating_cron_requests
	if r.Header.Get("X-Appengine-Cron") != "true" {
		http.NotFound(w, r)
		return
	}

	// getting all the recursers
	ctx := r.Context()
	recursersList, err := pl.rdb.GetAllUsers(ctx)
	if err != nil {
		log.Printf("Could not get list of recursers from DB: %s\n", err)
	}

	// botPassword, err := pl.adb.GetKey(ctx, "apiauth", "key")
	// if err != nil {
	// 	log.Println("Something weird happened trying to read the auth token from the database")
	// }

	accessToken, err := pl.adb.GetKey(ctx, "rc-accesstoken", "key")
	if err != nil {
		log.Printf("Something weird happened trying to read the RC API access token from the database: %s", err)
	}

	emailsOfPeopleAtRc := pl.rcapi.getCurrentlyActiveEmails(accessToken)

	for i := 0; i < len(recursersList); i++ {

		recurser := recursersList[i]

		recurserEmail := recurser.email
		recurserID := recurser.id

		isAtRCThisWeek := contains(emailsOfPeopleAtRc, recurserEmail)
		wasAtRCLastWeek := recursersList[i].currentlyAtRC

		log.Printf("User: %s was at RC last week: %t and is at RC this week: %t", recurserEmail, wasAtRCLastWeek, isAtRCThisWeek)

		//If they were at RC last week but not this week then we assume they have graduated or otherwise left RC
		//In that case we remove them from pairing bot so that inactive people do not get matched
		//If people who have left RC still want to use pairing bot, we give them the option to resubscribe
		if wasAtRCLastWeek && !isAtRCThisWeek {
			log.Printf("We would have unsubscribed the user: %s", recurserEmail)
			// var message string

			// err = pl.rdb.Delete(ctx, recurserID)
			// if err != nil {
			// 	log.Println(err)
			// 	message = fmt.Sprintf("Uh oh, I was trying to offboard you since it's the end of batch, but something went wrong. Consider messaging %v to let them know this happened.", owner)
			// } else {
			// 	log.Println("This user has been unsubscribed from pairing bot: ", recurserEmail)
			// 	message = offboardedMessage
			// }

			// err := pl.un.sendUserMessage(ctx, botPassword, recurserEmail, message)
			// if err != nil {
			// 	log.Printf("Error when trying to send offboarding message to %s: %s\n", recurserEmail, err)
			// }
		} else {
			log.Printf("We would NOT have unsubscribed the user: %s", recurserEmail)
		}

		recurser.currentlyAtRC = isAtRCThisWeek

		if err = pl.rdb.Set(ctx, recurserID, recurser); err != nil {
			log.Printf("Error encountered while update currentlyAtRC status for user: %s", recurserEmail)
		}
	}
}

func (pl *PairingLogic) checkin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	numPairings, err := pl.pdb.GetTotalPairingsDuringLastWeek(ctx)

	if err != nil {
		log.Println("Unable to get the total number of pairings durig the last week: : ", err)
	}

	recursersList, err := pl.rdb.GetAllUsers(ctx)
	if err != nil {
		log.Printf("Could not get list of recursers from DB: %s\n", err)
	}

	review, err := pl.revdb.GetRandom(ctx)
	if err != nil {
		log.Println("Could not get a random review from DB: ", err)
	}

	checkinMessage := getCheckinMessage(numPairings, len(recursersList), review.content)

	botPassword, err := pl.adb.GetKey(ctx, "apiauth", "key")
	if err != nil {
		log.Println("Something weird happened trying to read the auth token from the database")
	}

	err = pl.sm.postToTopic(ctx, botPassword, checkinMessage, "checkins", "Pairing Bot")

	if err != nil {
		log.Printf("Error when trying to submit Pairing Bot checkins stream message: %s\n", err)
	}
}

func getCheckinMessage(numPairings int, numRecursers int, review string) string {
	today := time.Now()
	todayFormatted := today.Format("January 2, 2006")

	message :=
		"```Bash\n" +
			"=> Initializing the Pairing Bot process\n" +
			"######################################################################## 100%%\n" +
			"=> Loading Pairing Bot Usage Statistics\n" +
			"######################################################################## 100%%\n" +
			"=> Teaching Pairing Bot how to boop beep boop as it is a strange loop\n" +
			"######################################################################## 00110001 00110000 00110000 00100101\n\n" +
			"``` \n\n\n" +
			"**%s Checkin**\n\n" +
			"* Current number of Recursers subscribed to Pairing Bot: %d\n\n" +
			"* Number of pairings facilitiated in the last week: %d \n\n" +
			"**Randomly Selected Pairing Bot Review**\n\n" +
			"* %s"

	return fmt.Sprintf(message, todayFormatted, numRecursers, numPairings, review)
}

/*
Sends out a "Welcome to Pairing Bot" message to 397 Bridge during the second week of RC to introduce people to RC.

We don't send this welcome message during the first week since it's a bit overwhelming with all of the orientation meetings
and people haven't had time to think too much about their projects.
*/
func (pl *PairingLogic) welcome(w http.ResponseWriter, r *http.Request) {
	// Check that the request is originating from within app engine
	// https://cloud.google.com/appengine/docs/flexible/go/scheduling-jobs-with-cron-yaml#validating_cron_requests
	if r.Header.Get("X-Appengine-Cron") != "true" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	accessToken, err := pl.adb.GetKey(ctx, "rc-accesstoken", "key")
	if err != nil {
		log.Printf("Something weird happened trying to read the RC API access token from the database: %s", err)
	}

	if pl.rcapi.isSecondWeekOfBatch(accessToken) {
		log.Println("the welcome cron would have posted a welcome message to Zulip")

		// ctx := r.Context()
		// botPassword, err := pl.adb.GetKey(ctx, "apiauth", "key")

		// if err != nil {
		// 	log.Println("Something weird happened trying to read the auth token from the database")
		// }

		// streamMessage := getWelcomeMessage()

		// err = pl.sm.postToTopic(ctx, botPassword, streamMessage, "397 Bridge", "🍐🤖")
		// if err != nil {
		// 	log.Printf("Error when trying to send welcome message about Pairing Bot %s\n", err)
		// }
	} else {
		log.Println("The welcome cron did not post a message to Zulip since it is not the second week of a batch")
	}
}

func getWelcomeMessage() string {
	today := time.Now()
	todayFormatted := today.Format("01.02.2006")

	message :=
		"```Bash\n" +
			"=> Initializing the Pairing Bot process\n" +
			"######################################################################## 100%%\n" +
			"=> Loading list of people currently at RC\n" +
			"######################################################################## 100%%\n" +
			"=> Teaching Pairing Bot how to beep boop beep\n" +
			"######################################################################## 00110001 00110000 00110000 00100101\n\n" +
			"=> Pairing Bot successfully updated to version %s\n" +
			"``` \n\n\n" +
			"Greetings @*Currently at RC*,\n\n" +
			"My name is Pairing Bot and my mission is to ~~eliminate all~~ help pair people at RC to work on projects.\n\n" +
			"**How To Get Started**\n\n" +
			"* Send me a private message with the word `subscribe` to get started. I will then match you with another pairing bot subscriber each day.\n\n" +
			"* Don't want to pair each day? You can set your schedule with the command `schedule tuesday friday` and I will only match you with people on those days.\n\n" +
			"* You can view a full list of my functions by sending me a PM with the message `help`.\n\n" +
			"**Have feedback/questions about Pairing Bot?**\n\n" +
			"* Please respond to this topic so I can better fulfill my duties of ~~throwing :pear: parties~~ connecting people together."

	return fmt.Sprintf(message, todayFormatted)
}
