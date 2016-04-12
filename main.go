package main

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sync"

	"golang.org/x/sys/unix"

	discord "github.com/bwmarrin/discordgo"
	"github.com/google/go-github/github"
)

/*
const (
	CREATE_INSTANT_INVITE = 0x0000001 //Allows creating of instant invites
	KICK_MEMBERS          = 0x0000002 //Allows kicking members
	BAN_MEMBERS           = 0x0000004 //Allows banning members
	MANAGE_ROLES          = 0x0000008 //Allows management and editing of roles
	MANAGE_CHANNELS       = 0x0000010 //Allows management and editing of channels
	MANAGE_GUILD          = 0x0000020 //Allows management and editing of the guild
	READ_MESSAGES         = 0x0000400 //Allows reading messages in a channel. The channel will not appear for users without this permission
	SEND_MESSAGES         = 0x0000800 //Allows for sending messages in a channel.
	SEND_TTS_MESSAGES     = 0x0001000 //Allows for sending of /tts messages
	MANAGE_MESSAGES       = 0x0002000 //Allows for deleting messages
	EMBED_LINKS           = 0x0004000 //Links sent by this user will be auto-embedded
	ATTACH_FILES          = 0x0008000 //Allows for uploading images and files
	READ_MESSAGE_HISTORY  = 0x0010000 //Allows for reading messages history
	MENTION_EVERYONE      = 0x0020000 //Allows for using the @everyone tag to notify all users in a channel
	CONNECT               = 0x0100000 //Allows for joining of a voice channel
	SPEAK                 = 0x0200000 //Allows for speaking in a voice channel
	MUTE_MEMBERS          = 0x0400000 //Allows for muting members in a voice channel
	DEAFEN_MEMBERS        = 0x0800000 //Allows for deafening of members in a voice channel
	MOVE_MEMBERS          = 0x1000000 //Allows for moving of members between voice channels
	USE_VAD               = 0x2000000 //Allows for using voice-activity-detection in a voice channel
)
*/

var (
	doserve    bool
	globaldone sync.WaitGroup
	channel    string
)

func startFifoMsgSrc(out chan string, quit chan struct{}) {
	//TODO: handle quit?
	osdatadir, ok := os.LookupEnv("OPENSHIFT_DATA_DIR")
	if !ok {
		osdatadir, _ = os.Getwd()
	}

	err := unix.Mkfifo(osdatadir+"/in", uint32(os.ModeNamedPipe|0600))
	if err != nil && err.Error() != "file exists" {
		panic(err)
	}

	globaldone.Add(1)
	go func() {
		for {
			in, err := os.Open(osdatadir + "/in")
			if err != nil {
				panic(err)
			}
			defer globaldone.Done()
			defer in.Close()

			inbuf := bufio.NewReader(in)

		readloop:
			for {
				line, err := inbuf.ReadString('\n')
				if err != nil {
					if err != io.EOF {
						log.Println("FIFO read error:", err.Error())
						return
					}
					break readloop
				}

				out <- line
			}
		}
	}()
}

func formatEvent(w http.ResponseWriter, evt string, pload []byte) (str string) {
	defer log.Printf("Sending gh message %s", str)
	var err error
	switch evt {
	case "push":
		pushevent := github.PushEvent{}
		if err = json.Unmarshal(pload, &pushevent); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}

		if *pushevent.Forced {
			str = fmt.Sprintf("%s FORCE pushed to %s", pushevent.Sender.Name, pushevent.Ref)
		} else {
			str = fmt.Sprintf("%s pushed to %s", pushevent.Sender.Name, pushevent.Ref)
		}
	case "delete":
		delete := github.DeleteEvent{}
		if err = json.Unmarshal(pload, &delete); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}
		str = fmt.Sprintf("%s deleted %s", delete.Sender.Name, delete.Ref)
	case "issues":
		issue := github.IssuesEvent{}
		if err = json.Unmarshal(pload, &issue); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}

		switch *issue.Action {
		case "opened":
			str = fmt.Sprintf("%s opened Issue %s", issue.Sender.Name, issue.Issue.Number)
			if issue.Assignee != nil {
				str = fmt.Sprintf("%s, assigned to %s", str, issue.Assignee.Name)
			}
			str = fmt.Sprintf("%s (%s)", str, issue.Issue.HTMLURL)
		case "closed":
			str = fmt.Sprintf("%s closed Issue %s (%s)", issue.Sender.Name, issue.Issue.Number, issue.Issue.HTMLURL)
		case "assigned":
			asnee := issue.Assignee.Name
			str = fmt.Sprintf("%s assigned Issue %s to %s (%s)", issue.Sender.Name, issue.Issue.Number, asnee, issue.Issue.HTMLURL)
		case "unassigned":
			asnee := issue.Assignee.Name
			str = fmt.Sprintf("%s unassigned Issue %s to %s (%s)", issue.Sender.Name, issue.Issue.Number, asnee, issue.Issue.HTMLURL)
		case "reopened":
			str = fmt.Sprintf("%s reopened Issue %s", issue.Sender.Name, issue.Issue.Number)
			str = fmt.Sprintf("%s (%s)", str, issue.Issue.HTMLURL)
		}
	case "pull_request":
		pr := github.PullRequestEvent{}
		if err = json.Unmarshal(pload, &pr); err != nil {
			http.Error(w, "malformed", http.StatusBadRequest)
			return
		}

		var action string
		switch *pr.Action {
		case "opened":
			action = "opened"
		case "closed":
			if *pr.PullRequest.Merged {
				action = "merged"
			} else {
				action = "closed"
			}
		case "reopened":
			action = "reopened"
		}
		if action == "" {
			return
		}
		str = fmt.Sprintf("%s %s PR %s (%s)", pr.Sender.Name, action, pr.Number, pr.PullRequest.HTMLURL)
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	return
}

func makeHandler(githubsecret string, out chan string, quit chan struct{}) func(w http.ResponseWriter, req *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "POST" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if req.URL.Path != "/hook" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		sig := req.Header.Get("X-Hub-Signature")
		evt := req.Header.Get("X-Github-Event")
		mac := hmac.New(sha1.New, []byte(githubsecret))

		pload, err := ioutil.ReadAll(req.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mac.Write([]byte(pload))
		expected := "sha1=" + hex.EncodeToString(mac.Sum(nil))
		eql := hmac.Equal([]byte(sig), []byte(expected))

		if !eql {
			w.WriteHeader(http.StatusUnauthorized)
			log.Printf("hook failed validation:%+v\n", req)
			return
		}

		str := formatEvent(w, evt, pload)

		if str != "" {
			w.Write([]byte(str))
			out <- str
		}
	}
}

func startHookMsgSrc(out chan string, quit chan struct{}) {
	githubsecret := os.Getenv("GITHUB_SECRET")
	log.Println("making handler")
	handlehook := makeHandler(githubsecret, out, quit)

	globaldone.Add(1)
	log.Println("starting http goroutine")
	go func() {
		defer globaldone.Done()
		bindto := fmt.Sprintf("%s:%s", os.Getenv("OPENSHIFT_GO_IP"), os.Getenv("OPENSHIFT_GO_PORT"))
		log.Println("Binding to", bindto)
		err := http.ListenAndServe(bindto, http.HandlerFunc(handlehook))

		if err != nil {
			log.Println("HOOK HANDLER DOWN:", err.Error())
		}
	}()
}

func msgloop(sess *discord.Session, me *discord.User, msgchan chan string, quit chan struct{}) {
	defer globaldone.Done()
	for {
		select {
		case <-quit:
			return
		case msg := <-msgchan:
			log.Println(msg)
			_, err := sess.ChannelMessageSend(channel, msg)
			if err != nil {
				log.Println("Error sending message %s: %s", msg, err.Error())
			}
		}
	}
}

func startMsgSink(token string) chan string {
	sess, err := discord.New(token)
	if err != nil {
		log.Fatal(err)
	}

	me, err := sess.User("@me")
	if err != nil {
		log.Fatal(err)
	}

	msgchan := make(chan string)

	globaldone.Add(1)
	go msgloop(sess, me, msgchan, nil)

	return msgchan
}

func init() {
	flag.BoolVar(&doserve, "s", true, "doodle")
	flag.Parse()

	var ok bool
	if channel, ok = os.LookupEnv("DISCORD_CHANNEL"); !ok {
		log.Fatal("Need a channel")
	}
}

func main() {
	msgchan := startMsgSink(os.Getenv("BOT_TOKEN"))

	log.Println("Starting fifo msg src")
	startFifoMsgSrc(msgchan, nil)

	if doserve {
		log.Println("Starting http")
		startHookMsgSrc(msgchan, nil)
	}
	log.Println("Waiting")
	globaldone.Wait()
}

/*
func logstuff() {
	logmsg := func(s *discord.Session, m *discord.Message) {
		log.Printf("%+v\n", m) //m.ID, m.ChannelID, m.Content, m.Timestamp, m.EditedTimestamp, m.Tts, m.MentionEveryone, *m.Author, m.Attachments, m.Embeds, m.Mentions)

		for _, emb := range m.Embeds {
			log.Printf("%+v\n", emb)
			if emb.Provider != nil {
				log.Printf("%+v\n", emb.Provider)
			}
		}

		for _, mns := range m.Mentions {
			log.Printf("Got %+v, I'm %+v", mns, me)

			if mns.ID == me.ID {
				s.ChannelMessageSend(m.ChannelID, "sup?")
			}
		}

		channel, ok := chans[m.ChannelID]

		if !ok {
			channel, err = s.Channel(m.ChannelID)
			if err != nil {
				log.Fatal(err)
			}
			chans[m.ChannelID] = channel
		}

		log.Printf("%+v, rec: %+v\n", channel, channel.Recipient)

		giild, _ := s.Guild(channel.GuildID)

		log.Printf("%+v\n", giild)
		if channel.IsPrivate {
			room = m.ChannelID
		}
	}

	logguild := func(s *discord.Session, g *discord.Guild) {
		log.Printf("%+v\n", g)
	}

	sess.AddHandler(func(s *discord.Session, m *discord.MessageCreate) { logmsg(s, m.Message) })
	sess.AddHandler(func(s *discord.Session, m *discord.GuildCreate) { logguild(s, m.Guild) })
	sess.AddHandler(func(s *discord.Session, m *discord.GuildUpdate) { logguild(s, m.Guild) })
	sess.AddHandler(func(s *discord.Session, m *discord.GuildDelete) { logguild(s, m.Guild) })

	sess.Open()
}
*/
