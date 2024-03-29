package main

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/docopt/docopt.go"
)

const (
	loggerDirname       = "log/"
	remindersDirname    = "reminders/"
	remindersFilePrefix = "reminders-"
	remindersFileSuffix = ".csv"
)

var logger *log.Logger
var stop = make(chan struct{})

var internalErrMsg = &discordgo.MessageSend{
	Content: "internal error",
}

func sendMsg(s *discordgo.Session, channelID string, msg string) {
	_, err := s.ChannelMessageSend(channelID, msg)
	if err != nil {
		logger.Printf("sending message %v: %v\n", msg, err)
	}
}

func sendMsgCmplx(s *discordgo.Session, channelID string, msg *discordgo.MessageSend) {
	_, err := s.ChannelMessageSendComplex(channelID, msg)
	if err != nil {
		logger.Printf("sending message %v: %v\n", msg, err)
	}
}

func addReaction(s *discordgo.Session, channelID string, messageID string, emoji string) {
	err := s.MessageReactionAdd(channelID, messageID, emoji)
	if err != nil {
		logger.Printf("adding reaction %v: %v\n", emoji, err)
	}
}

type userLog discordgo.User

func (u *userLog) String() string {
	return fmt.Sprintf("@%s (id: %s)",
		(*discordgo.User)(u).String(), u.ID)
}

type reminder struct {
	userID     string
	creation   time.Time
	expiration time.Time
	message    string
}

func (r *reminder) String() string {
	return fmt.Sprintf("%s,%s,%s,%q",
		r.userID,
		r.creation.Format(time.RFC3339Nano),
		r.expiration.Format(time.RFC3339Nano),
		r.message,
	)
}

type remindmeState struct {
	reminders []*reminder
	timers    []*time.Timer
	session   *discordgo.Session
	*sync.Mutex
}

var rmState remindmeState

func (rs *remindmeState) Add(r *reminder) {
	sendReminder := func() {
		user, err := rs.session.User(r.userID)
		if err != nil {
			logger.Printf("unable to open private channel with %s to send the message \"%s\": %v",
				r.userID, r.message, err)
			return
		}
		dm, err := rs.session.UserChannelCreate(user.ID)
		if err != nil {
			logger.Printf("unable to open private channel with %s to send the message \"%s\": %v",
				(*userLog)(user), r.message, err)
			return
		}
		sendMsg(rs.session, dm.ID, fmt.Sprintf("Reminder from %s: %s", r.creation, r.message))
		logger.Printf("Sent reminder for %s created %s with the message \"%s\"",
			(*userLog)(user), r.creation, r.message)
	}
	fromNow := time.Until(r.expiration)
	if int64(fromNow) <= 1 {
		sendReminder()
		return
	}
	rs.Lock()
	userID, expiration := r.userID, r.expiration
	t := time.AfterFunc(fromNow, func() {
		sendReminder()
		rs.Remove(userID, expiration)
	})
	i := sort.Search(len(rs.reminders), func(i int) bool {
		return rs.reminders[i].userID > r.userID
	})
	rs.reminders = append(rs.reminders, nil)
	copy(rs.reminders[i+1:], rs.reminders[i:])
	rs.reminders[i] = r
	rs.timers = append(rs.timers, nil)
	copy(rs.timers[i+1:], rs.timers[i:])
	rs.timers[i] = t
	rs.Unlock()
}

func (rs *remindmeState) Remove(userID string, expiration time.Time) bool {
	rs.Lock()
	defer rs.Unlock()
	i := sort.Search(len(rmState.reminders), func(i int) bool {
		return rmState.reminders[i].userID >= userID
	})
	j := sort.Search(len(rmState.reminders), func(i int) bool {
		return rmState.reminders[i].userID > userID
	})
	if j - i == 0 {
		logger.Print("Reminder for removal not found.")
		return false
	}
	authorReminders := rmState.reminders[i:j]
	k := sort.Search(len(authorReminders), func(i int) bool {
		return authorReminders[i].expiration.Before(expiration)
	})
	k--
	if k == -1 || !authorReminders[k].expiration.Equal(expiration) {
		logger.Print("Reminder for removal not found.")
		return false
	}
	k += i
	if !rs.timers[k].Stop() {
		logger.Print("Reminder for removal already triggering.")
		return false
	}
	rs.reminders[k] = nil
	copy(rs.reminders[k:], rs.reminders[k+1:])
	rs.reminders = rs.reminders[:len(rs.reminders)-1]
	rs.timers[k] = nil
	copy(rs.timers[k:], rs.timers[k+1:])
	rs.timers = rs.timers[:len(rs.timers)-1]
	logger.Printf("Removed reminder for %s to go off %s", userID, expiration)
	return true
}

func (rs *remindmeState) ReadFrom(r io.Reader) (int64, error) {
	bb := new(bytes.Buffer)
	n, err := bb.ReadFrom(r)
	if err != nil {
		return n, err
	}
	rr := csv.NewReader(bb)
	rr.ReuseRecord = true
	for {
		record, err := rr.Read()
		if err != nil {
			if err == io.EOF {
				return n, nil
			}
			return n, err
		}
		r := new(reminder)
		r.userID = record[0]
		r.creation, err = time.Parse(time.RFC3339Nano, record[1])
		if err != nil {
			return n, fmt.Errorf("invalid reminder record: %s", record)
		}
		r.expiration, err = time.Parse(time.RFC3339Nano, record[2])
		if err != nil {
			return n, fmt.Errorf("invalid reminder record: %s", record)
		}
		r.message = record[3]
		rs.Add(r)
	}
}

func (rs *remindmeState) WriteTo(w io.Writer) (int64, error) {
	bb := new(bytes.Buffer)
	for _, r := range rs.reminders {
		bb.WriteString(r.String())
		bb.WriteByte('\n')
	}
	return io.Copy(w, bb)
}

func constructRMState(s *discordgo.Session) error {
	rmState.session = s
	rmState.Mutex = new(sync.Mutex)
	remindersDir, err := os.Open(remindersDirname)
	if err != nil {
		return fmt.Errorf("unable to open reminders directory: %v", err)
	}
	defer remindersDir.Close()
	reminderFiles, err := remindersDir.Readdirnames(0)
	if err != nil {
		return fmt.Errorf("unable to access reminders directory: %v", err)
	}
	if len(reminderFiles) == 0 {
		return fmt.Errorf("no reminder files found")
	}
	sort.Strings(reminderFiles)
	remindersFile, err := os.Open(
		filepath.Join(remindersDirname, reminderFiles[len(reminderFiles)-1]))
	if err != nil {
		return fmt.Errorf("unable to open reminders file: %v", err)
	}
	_, err = rmState.ReadFrom(remindersFile)
	if err != nil {
		for i := range rmState.reminders {
			rmState.reminders[i] = nil
		}
		rmState.reminders = rmState.reminders[:0]
		for i := range rmState.timers {
			rmState.timers[i].Stop()
			rmState.timers[i] = nil
		}
		rmState.timers = rmState.timers[:0]
		logger.Print("unable to import reminders file: ", err)
	}
	remindersFile.Close()
	return nil
}

func deconstructRMState() {
	rmState.Lock()
	for _, timer := range rmState.timers {
		timer.Stop()
	}
	rmState.Unlock()
	err := os.Mkdir(remindersDirname, 0700)
	if err != nil && !os.IsExist(err) {
		logger.Print("unable to create reminders directory", err)
		logger.Print("aborting records to stderr")
		rmState.WriteTo(os.Stderr)
		return
	}
	remindersFile, _ := os.Create(
		remindersDirname + remindersFilePrefix +
			time.Now().In(time.UTC).Format(time.RFC3339) +
			remindersFileSuffix)
	rmState.WriteTo(remindersFile)
	err = remindersFile.Close()
	if err != nil {
		logger.Print("error exporting reminders: ", err)
	}
}

func newRemindmeParser(s *discordgo.Session, channelID string) *docopt.Parser {
	parser := new(docopt.Parser)
	parser.HelpHandler = func(err error, usage string) {
		var errStr string
		if err != nil {
			errStr = err.Error()
		}
		buflen := len(errStr) + len(usage)
		buf := make([]byte, buflen)
		n := copy(buf, errStr)
		copy(buf[n:], usage)
		sendMsg(s, channelID, string(buf))
	}
	return parser
}

func remindmeHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	const remindmeUsage = `
Usage:
	!remindme list
	!remindme cancel <expiration>
	!remindme <duration> [-c|--withcontext] <message>...
`
	m.Content = strings.TrimLeftFunc(m.Content, unicode.IsSpace)
	if m.Content == "" || !strings.HasPrefix(m.Content, "!remindme") {
		return
	}
	argv := strings.Fields(m.Content)
	parser := newRemindmeParser(s, m.ChannelID)
	opts, err := parser.ParseArgs(remindmeUsage, argv[1:], "")
	if err != nil {
		if _, ok := err.(*docopt.UserError); !ok {
			logger.Panic("invalid option parser: ", err)
		}
		return
	}
	var remindmeConfig struct {
		List        bool
		Cancel      bool
		Expiration  string
		Duration    string
		WithContext bool `docopt:"-c,--withcontext"`
		Message     []string
	}
	err = opts.Bind(&remindmeConfig)
	if err != nil {
		logger.Panic("unable to bind options: ", err)
		return
	}
	logger.Printf("User %s sent command \"%s\"", (*userLog)(m.Author), m.Content)
	switch {
	case remindmeConfig.List:
		authorID := m.Author.ID
		rmState.Lock()
		defer rmState.Unlock()
		i := sort.Search(len(rmState.reminders), func(i int) bool {
			return rmState.reminders[i].userID >= authorID
		})
		j := sort.Search(len(rmState.reminders), func(i int) bool {
			return rmState.reminders[i].userID > authorID
		})
		if j - i == 0 {
			sendMsg(s, m.ChannelID, "you have no reminders")
			return
		}
		dm, err := s.UserChannelCreate(authorID)
		if err != nil {
			logger.Printf("unable to open private channel with %s for list command: %v",
				(*userLog)(m.Author), err)
			return
		}
		const listFmt = "`%s` :small_blue_diamond: `%s` :small_blue_diamond: `%s`\n"
		list := new(strings.Builder)
		list.WriteString(fmt.Sprintf(listFmt, "creation", "expiration", "message"))
		for _, r := range rmState.reminders[i:j] {
			list.WriteString(fmt.Sprintf(listFmt,
				r.creation.Format(time.RFC3339Nano),
				r.expiration.Format(time.RFC3339Nano),
				r.message,
			))
		}
		sendMsg(s, dm.ID, list.String())
	case remindmeConfig.Cancel:
		expiration, err := time.Parse(time.RFC3339Nano, remindmeConfig.Expiration)
		if err != nil {
			parser.HelpHandler(err, remindmeUsage)
			return
		}
		if rmState.Remove(m.Author.ID, expiration) {
			addReaction(s, m.ChannelID, m.ID, "✅")
		} else {
			addReaction(s, m.ChannelID, m.ID, "❌")
		}
	default:
		author := m.Author
		creation := time.Now().In(time.UTC)
		duration, err := parseDuration(remindmeConfig.Duration)
		if err != nil {
			parser.HelpHandler(err, remindmeUsage)
			return
		}
		expiration := creation.Add(duration)
		if remindmeConfig.WithContext {
			remindmeConfig.Message = append(remindmeConfig.Message,
				fmt.Sprintf("\nContext: https://discordapp.com/channels/%s/%s/%s",
					m.GuildID, m.ChannelID, m.ID))
		}
		message := strings.Join(remindmeConfig.Message, " ")
		r := &reminder{
			userID:     author.ID,
			creation:   creation,
			expiration: expiration,
			message:    message,
		}
		rmState.Add(r)
		logger.Printf("Set reminder for %s to go off %s with the message %q",
			(*userLog)(m.Author), expiration, message)
		addReaction(s, m.ChannelID, m.ID, "🆗")
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: remindme <botToken>")
		os.Exit(1)
	}
	botToken := os.Args[1]

	// Logging
	err := os.Mkdir(loggerDirname, 0700)
	if err != nil && !os.IsExist(err) {
		panic(fmt.Errorf("unable to create logger directory: %v", err))
	}
	logFile, err := os.Create(loggerDirname + time.Now().In(time.UTC).Format(time.RFC3339))
	logger = log.New(logFile,
		"", log.Ldate|log.Lmicroseconds|log.Lshortfile|log.LUTC)
	if err != nil {
		logger.Panic("creating logfile: ", err)
	}
	defer func() {
		err = logFile.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "closing logfile: ", err)
		}
	}()
	// Signal handler
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, os.Interrupt, os.Kill)
		<-sigs
		stop <- struct{}{}
	}()
	// Terminal
	go func() {
		fmt.Println("Say \"stop\" to quit.")
		var echo string
		for echo != "stop" {
			fmt.Scanln(&echo)
		}
		stop <- struct{}{}
	}()
	// REST API
	go func() {
		http.HandleFunc("/", func(_ http.ResponseWriter, req *http.Request) {
			ls := len("stop")
			buf := make([]byte, ls)
			n, _ := req.Body.Read(buf)
			if n == ls && string(buf) == "stop" {
				stop <- struct{}{}
			}
		})
		logger.Panic(http.ListenAndServe(":6767", nil))
	}()
	// Bot session
	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		logger.Panic(err)
	}
	err = session.Open()
	if err != nil {
		logger.Panic(err)
	}
	logger.Print("Session opened.")
	defer func() {
		err = session.Close()
		if err != nil {
			logger.Print(err)
		}
		logger.Print("Session closed.")
	}()
	// Construct remindmeState
	err = constructRMState(session)
	if err != nil {
		logger.Print(err)
	}
	defer deconstructRMState()
	// Register handler
	session.AddHandler(remindmeHandler)

	<-stop
}
