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

	"github.com/bwmarrin/discordgo"
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
	userID   string
	creation time.Time
	duration time.Duration
	message  string
}

func (r *reminder) String() string {
	return fmt.Sprintf("%s,%s,%s,%q",
		r.userID,
		r.creation.Format(time.RFC3339Nano),
		r.duration,
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
		}
		dm, err := rs.session.UserChannelCreate(user.ID)
		if err != nil {
			logger.Printf("unable to open private channel with %s to send the message \"%s\": %v",
				(*userLog)(user), r.message, err)
			return
		}
		sendMsg(rs.session, dm.ID, fmt.Sprintf("%s Message from %s: %s", user, r.creation, r.message))
		logger.Printf("Sent reminder for %s created %s with the message \"%s\"",
			(*userLog)(user), r.creation, r.message)
	}
	expiration := r.creation.Add(r.duration)
	fromNow := time.Until(expiration)
	if int64(fromNow) <= 1 {
		sendReminder()
		return
	}
	rs.Lock()
	rs.reminders = append(rs.reminders, r)
	rs.timers = append(rs.timers, time.AfterFunc(fromNow, func() {
		sendReminder()
		rs.Lock()
		var k int = -1
		for i := range rs.reminders {
			if rs.reminders[i] == r {
				k = i
				break
			}
		}
		if k < 0 {
			logger.Panic("unmatched timer")
		}
		rs.reminders[k] = nil
		copy(rs.reminders[k:], rs.reminders[k+1:])
		rs.reminders = rs.reminders[:len(rs.reminders)-1]
		rs.timers[k] = nil
		copy(rs.timers[k:], rs.timers[k+1:])
		rs.timers = rs.timers[:len(rs.timers)-1]
		rs.Unlock()
	}))
	rs.Unlock()
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
		r.duration, err = time.ParseDuration(record[2])
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
			time.Now().Format(time.RFC3339) +
			remindersFileSuffix)
	rmState.WriteTo(remindersFile)
	err = remindersFile.Close()
	if err != nil {
		logger.Print("error exporting reminders: ", err)
	}
}

func remindmeHandler(s *discordgo.Session, m *discordgo.MessageCreate) {
	m.Content = strings.TrimSpace(m.Content)
	if len(m.Content) == 0 {
		return
	}
	if m.Content[0] != '!' {
		return
	}
	i := strings.Index(m.Content, " ")
	if i < 0 {
		i = len(m.Content)
	}
	cmd := m.Content[:i]
	if cmd != "!remindme" {
		return
	}
	logger.Printf("User %s sent command \"%s\"", (*userLog)(m.Author), m.Content)
	author := m.Author
	var remindmeUsageMsg = author.Mention() + " Usage: !remindme <duration> <message>..."
	m.Content = strings.TrimSpace(m.Content[i:])
	i = strings.Index(m.Content, " ")
	if i < 0 {
		sendMsg(s, m.ChannelID, remindmeUsageMsg)
		return
	}
	durationString := m.Content[:i]
	m.Content = strings.TrimSpace(m.Content[i:])
	duration, err := parseDuration(durationString)
	if err != nil {
		sendMsg(s, m.ChannelID, "remindme: " + err.Error())
		sendMsg(s, m.ChannelID, remindmeUsageMsg)
		return
	}
	msg := m.Content
	if msg == "" {
		sendMsg(s, m.ChannelID, remindmeUsageMsg)
		return
	}
	r := &reminder{
		userID:   author.ID,
		creation: time.Now().In(time.UTC),
		duration: duration,
		message:  msg,
	}
	rmState.Add(r)
	logger.Printf("Set reminder for %s to go off %s with the message \"%s\"",
		(*userLog)(m.Author), r.creation.Add(duration), msg)
	//sendMsg(s, m.ChannelID, author.Mention()+" OK")
	addReaction(s, m.ChannelID, m.ID, "🆗")
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
	logFile, err := os.Create(loggerDirname + time.Now().Format(time.RFC3339))
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
