package main // in Zukunft ---> Das pongStrong package

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	"cloud.google.com/go/storage"
	"github.com/gorilla/sessions"
)

const (
	// local file paths
	teamsPath    string = "db/teams.json"
	gruppenPath  string = "db/gruppen.json"
	tabellenPath string = "db/tabellen.json"
	knockoutPath string = "db/kophase.json"
	schlangePath string = "db/schlange.json"

	// cloud object names
	cloudStorageBucket string = "pongstrong-backup-bucket"
	teamsCloudPath     string = "teams.json"
	schlangeCloudPath  string = "schlangestate.json"
	gruppenCloudPath   string = "gruppenstate.json"
	knockoutCloudPath  string = "kophasestate.json"
)

var tmpl *template.Template

// ReadFromFile tries to read the data from path as json to struct obj
func ReadFromFile[T any](obj T, path string) T {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println(err)
	}
	err = json.Unmarshal(data, obj)
	if err != nil {
		fmt.Println(err)
	}
	return obj
}

// WriteToFile tries to write the data from obj to path as json
func WriteToFile[T any](obj T, path string) {
	data, err := json.MarshalIndent(obj, "", "\t")
	if err != nil {
		fmt.Println(err)
	}
	err = os.WriteFile(path, data, fs.ModePerm)
	if err != nil {
		fmt.Println(err)
	}
}

type Teams [][4]Team

type Team struct {
	Name string `json:"name"`
	Mem1 string `json:"mem1"`
	Mem2 string `json:"mem2"`
}

// origin returns the group the team is in
func (team *Team) origin() int {
	teams := *ReadFromFile(new(Teams), teamsPath)
	for g, t := range teams {
		for i := 0; i < 4; i++ {
			if t[i] == *team {
				return g
			}
		}
	}
	return -1
}

type Match struct {
	Team1   Team   `json:"team1"`
	Team2   Team   `json:"team2"`
	Score1  int    `json:"score1"`
	Score2  int    `json:"score2"`
	TischNr int    `json:"tischnummer"`
	ID      string `json:"id"`
	Done    bool   `json:"done"`
}

// winner gibt das gewinnerteam zurück
func winner(match Match) (Team, error) {
	// deathcup
	if match.Score1 < 0 {
		return match.Team1, nil
	} else if match.Score2 < 0 {
		return match.Team2, nil
	}
	// normal
	if match.Score1 > match.Score2 {
		return match.Team1, nil
	} else if match.Score2 > match.Score1 {
		return match.Team2, nil
	}
	return Team{}, errors.New("scores are invalid")
}

// points gibt die Punkte der beiden beteiligten Teams zurück
func points(match Match) (int, int, error) {
	if match.Score1 == 0 && match.Score2 == 0 {
		return 0, 0, nil
	}

	winner := [4]int{3, 2, 4, 3}
	looser := [4]int{0, 1, 0, 1}

	// deathcup
	if match.Score1 == -1 {
		return winner[2], looser[2], nil
	} else if match.Score2 == -1 {
		return looser[2], winner[2], nil
	}
	// deathcup overtime
	if match.Score1 == -2 {
		return winner[3], looser[3], nil
	} else if match.Score2 == -2 {
		return looser[3], winner[3], nil
	}
	// normal
	if match.Score1 == 10 && match.Score2 < 10 {
		return winner[0], looser[0], nil
	} else if match.Score2 == 10 && match.Score1 < 10 {
		return looser[0], winner[0], nil
	}
	// overtime
	if match.Score1 >= 10 && match.Score2 >= 10 && match.Score1 > match.Score2 {
		return winner[1], looser[1], nil
	} else if match.Score1 >= 10 && match.Score2 >= 10 && match.Score2 > match.Score1 {
		return looser[1], winner[1], nil
	}
	return 0, 0, errors.New("scores are invalid")
}

// Gruppen hält alle Infos über die Gruppen-Phase
type Gruppen [][]Match

// createGruppen erstellt aus dem Teams struct eine Gruppen struct
func CreateGruppen(teams Teams) (Gruppen, error) {

	// create the Gruppen struct
	length := len(teams)
	groups := make(Gruppen, length)
	for i := 0; i < length; i++ {
		groups[i] = make([]Match, 6)
	}

	// use pairing pattern to generate matches
	pattern := []int{0, 1, 2, 3, 0, 2, 1, 3, 3, 0, 1, 2}
	for i := 0; i < length; i++ {
		k := 0
		for j := 0; j < 6; j++ {
			groups[i][j].Team1 = teams[i][pattern[k]]
			k++
			groups[i][j].Team2 = teams[i][pattern[k]]
			k++
			groups[i][j].ID = "g" + strconv.Itoa(i+1) + strconv.Itoa(j+1)
		}
	}

	// use table blueprint to set the matches deskS
	blueprint := [6][6]int{
		{1, 2, 5, 6, 3, 4},
		{3, 4, 1, 2, 5, 6},
		{5, 6, 3, 4, 1, 2},
		{1, 2, 5, 6, 3, 4},
		{3, 4, 1, 2, 5, 6},
		{5, 6, 3, 4, 1, 2},
	}

	for i := 0; i < len(blueprint[0]); i++ {
		for j := 0; j < len(blueprint); j++ {
			groups[j][i].TischNr = blueprint[j][i]
		}
	}

	return groups, nil
}

// Knockouts hält alle Infos über die Knockout-Phase
type Knockouts struct {
	Champions  Champions  `json:"champions"`
	Europa     Europa     `json:"europa"`
	Conference Conference `json:"conference"`
	Super      Super      `json:"super"`
}

func (k *Knockouts) Instatiate() {
	k.Champions.instatiate()
	k.Europa.Instatiate()
	k.Conference.Instatiate()
	k.Super.Instatiate()
}

// update checks for finnished matches and moves teams to the next round
func (knock *Knockouts) update() {
	if knock.Champions[0][0].Team1.Name == "" && knock.Champions[0][0].Team2.Name == "" {
		return // Knockouts have not started yet
	}
	// champ
	for i := 0; i < len(knock.Champions)-1; i++ {
		for j := 0; j < len(knock.Champions[i]); j++ {
			if knock.Champions[i][j].Done {
				if j%2 == 0 {
					knock.Champions[i+1][j/2].Team1, _ = winner(knock.Champions[i][j])
				} else {
					knock.Champions[i+1][j/2].Team2, _ = winner(knock.Champions[i][j])
				}
			}
		}
	}
	// euro
	for i := 0; i < len(knock.Europa)-1; i++ {
		for j := 0; j < len(knock.Europa[i]); j++ {
			if knock.Europa[i][j].Done {
				if j%2 == 0 {
					knock.Europa[i+1][j/2].Team1, _ = winner(knock.Europa[i][j])
				} else {
					knock.Europa[i+1][j/2].Team2, _ = winner(knock.Europa[i][j])
				}
			}
		}
	}
	// conf
	for i := 0; i < len(knock.Conference)-1; i++ {
		for j := 0; j < len(knock.Conference[i]); j++ {
			if knock.Conference[i][j].Done {
				if j%2 == 0 {
					knock.Conference[i+1][j/2].Team1, _ = winner(knock.Conference[i][j])
				} else {
					knock.Conference[i+1][j/2].Team2, _ = winner(knock.Conference[i][j])
				}
			}
		}
	}
	// super
	if knock.Super[0].Done {
		t, err := winner(knock.Super[0])
		if err != nil {
			log.Println(err)
		}
		knock.Super[1].Team2 = t
	}

	// move league winners to super
	if knock.Europa[len(knock.Europa)-1][0].Done {
		t, err := winner(knock.Europa[len(knock.Europa)-1][0])
		if err != nil {
			log.Println(err)
		} else {
			knock.Super[0].Team1 = t
		}
	}
	if knock.Conference[len(knock.Conference)-1][0].Done {
		t, err := winner(knock.Conference[len(knock.Conference)-1][0])
		if err != nil {
			log.Println(err)
		} else {
			knock.Super[0].Team2 = t
		}
	}
	if knock.Champions[len(knock.Champions)-1][0].Done {
		t, err := winner(knock.Champions[len(knock.Champions)-1][0])

		if err != nil {
			log.Println(err)
		} else {
			knock.Super[1].Team1 = t
		}
	}
}

type Champions [4][]Match

func (champ *Champions) instatiate() {
	champ[0] = make([]Match, 8)
	champ[1] = make([]Match, 4)
	champ[2] = make([]Match, 2)
	champ[3] = make([]Match, 1)
	for i := 0; i < 4; i++ {
		for j := 0; j < len(champ[i]); j++ {
			champ[i][j] = Match{ID: "c" + strconv.Itoa(i+1) + strconv.Itoa(j+1)}
		}
	}
}

type Europa [3][]Match

func (euro *Europa) Instatiate() {
	euro[0] = make([]Match, 4)
	euro[1] = make([]Match, 2)
	euro[2] = make([]Match, 1)
	for i := 0; i < 3; i++ {
		for j := 0; j < len(euro[i]); j++ {
			euro[i][j] = Match{ID: "e" + strconv.Itoa(i+1) + strconv.Itoa(j+1)}
		}
	}
}

type Conference [3][]Match

func (conf *Conference) Instatiate() {
	conf[0] = make([]Match, 4)
	conf[1] = make([]Match, 2)
	conf[2] = make([]Match, 1)
	for i := 0; i < 3; i++ {
		for j := 0; j < len(conf[i]); j++ {
			conf[i][j] = Match{ID: "f" + strconv.Itoa(i+1) + strconv.Itoa(j+1)}
		}
	}
}

type Super [2]Match

func (sup *Super) Instatiate() {
	sup[0] = Match{ID: "s1"}
	sup[1] = Match{ID: "s2"}
}

type Tabellen [][4]Row

type Row struct {
	Team      Team      `json:"team"`
	Punkte    int       `json:"punkte"`
	Differenz int       `json:"differenz"`
	Becher    int       `json:"becher"`
	Vergleich [4]string `json:"vergleich"`
}

// SortTable sortiert eine zu einer Gruppe zugehörige Bewertungstabelle
func SortTable(r []Row) {
	sort.SliceStable(r, func(i, j int) bool {
		if r[i].Punkte < r[j].Punkte {
			return false
		} else if r[i].Punkte == r[j].Punkte && r[i].Differenz < r[j].Differenz {
			return false
		} else if r[i].Punkte == r[j].Punkte && r[i].Differenz == r[j].Differenz && r[i].Becher < r[j].Becher {
			return false
		} else if r[i].Punkte == r[j].Punkte && r[i].Differenz == r[j].Differenz && r[i].Becher == r[j].Becher {
			for _, v := range r[i].Vergleich {
				if v == r[j].Team.Name {
					return false
				}
			}
		}
		return true
	})
}

// SortTables sortiert alle Bewertungstabellen einer Gruppen-Phase
func sortTables(t Tabellen) {
	for i := 0; i < len(t); i++ {
		tHelp := t[i][:]
		SortTable(tHelp)
		copy(t[i][:], tHelp)
	}
}

// MatchQueue hält die Aktuell anstehenden und laufenden Matches
type MatchQueue struct {
	Waiting [][]Match
	Playing []Match
}

// switchPlaying moves the match at index from Waiting to Playing
func (q *MatchQueue) switchPlaying(index int) error {
	m := q.Waiting[index][0]
	if len(q.Waiting[index]) == 0 {
		return errors.New("*MatchQueue.SwitchPlaying: no elements in line")
	}
	if !q.isFree(index + 1) {
		return errors.New("*MatchQueue.SwitchPlaying: table is occupied")
	}
	q.Waiting[index] = q.Waiting[index][1:]
	q.Playing = append(q.Playing, m)
	return nil
}

// remove removes the Match from the MatchQueue
func (q *MatchQueue) removeFromPlaying(id int) error {
	for i, match := range q.Playing {
		if match.TischNr == id {
			q.Playing = append(q.Playing[:i], q.Playing[i+1:]...)
			return nil
		}
	}
	return errors.New("did not find match in Queue")
}

// Next returns the first Match from Queue index
func (q *MatchQueue) Next(index int) *Match {
	if len(q.Waiting[index]) == 0 {
		return nil
	}
	return &q.Waiting[index][0]
}

// nextMatches returns all Matches with unoccupied table
func (q *MatchQueue) nextMatches() []Match {
	matches := []Match{}
	for i := 0; i < len(q.Waiting); i++ {
		match := q.Next(i)
		if match != nil && q.isFree(match.TischNr) {
			matches = append(matches, *match)
		}
	}
	return matches
}

// NextNextMatch returns all Matches with occupied table
func (q *MatchQueue) NextNextMatches() []Match {
	matches := []Match{}
	for i := 0; i < len(q.Waiting); i++ {
		match := q.Next(i)
		if match != nil && !q.isFree(match.TischNr) {
			matches = append(matches, *match)
		}
	}
	return matches
}

// getMatchID returns the ID of the Match at table t if in Playing
func (q *MatchQueue) getMatchID(index int) (string, error) {
	for _, match := range q.Playing {
		if match.TischNr == index {
			return match.ID, nil
		}
	}
	return "", errors.New("*MatchQueue.getMatchID: table not occupied")
}

// contains checks if a Match is already in the MatchQueue
func (q *MatchQueue) contains(match Match) bool {
	for _, lines := range q.Waiting {
		for _, m := range lines {
			if match == m {
				return true
			}
		}
	}
	for _, m := range q.Playing {
		if match == m {
			return true
		}
	}
	return false
}

// isFree checks if table index is free
func (q *MatchQueue) isFree(index int) bool {
	for _, m := range q.Playing {
		if m.TischNr == index {
			return false
		}
	}
	return true
}

// isEmpty returns true if q is completely empty
func (q *MatchQueue) isEmpty() bool {
	for _, v := range q.Waiting {
		if len(v) > 0 {
			return false
		}
	}
	return len(q.Playing) == 0
}

// updateKnockQueue adds new ready Matches to the matchQueue
func (q *MatchQueue) updateKnockQueue() {
	knock := ReadFromFile(new(Knockouts), knockoutPath)
	if knock.Champions[0][0].Team1.Name == "" && knock.Champions[0][0].Team2.Name == "" {
		return
	}

	matchReady := func(m Match, q *MatchQueue) bool {
		return !m.Done && m.Team1.Name != "" && m.Team2.Name != "" && !q.contains(m)
	}

	// search for new matches
	for i := 0; i < len(knock.Champions); i++ {
		for j := 0; j < len(knock.Champions[i]); j++ {
			if matchReady(knock.Champions[i][j], q) {
				q.Waiting[knock.Champions[i][j].TischNr-1] = append(q.Waiting[knock.Champions[i][j].TischNr-1], knock.Champions[i][j])
			}
		}
	}
	for i := 0; i < len(knock.Europa); i++ {
		for j := 0; j < len(knock.Europa[i]); j++ {
			if matchReady(knock.Europa[i][j], q) {
				q.Waiting[knock.Europa[i][j].TischNr-1] = append(q.Waiting[knock.Europa[i][j].TischNr-1], knock.Europa[i][j])
			}
		}
	}
	for i := 0; i < len(knock.Conference); i++ {
		for j := 0; j < len(knock.Conference[i]); j++ {
			if matchReady(knock.Conference[i][j], q) {
				q.Waiting[knock.Conference[i][j].TischNr-1] = append(q.Waiting[knock.Conference[i][j].TischNr-1], knock.Conference[i][j])
			}
		}
	}
	for i := 0; i < 2; i++ {
		if matchReady(knock.Super[i], q) {
			q.Waiting[knock.Super[i].TischNr-1] = append(q.Waiting[knock.Super[i].TischNr-1], knock.Super[i])
		}
	}

	// move league winners to super
	if knock.Europa[len(knock.Europa)-1][0].Done {
		t, err := winner(knock.Europa[len(knock.Europa)-1][0])
		if err != nil {
			log.Println(err)
		} else {
			knock.Super[0].Team1 = t
		}
	}
	if knock.Conference[len(knock.Conference)-1][0].Done {
		t, err := winner(knock.Conference[len(knock.Conference)-1][0])
		if err != nil {
			log.Println(err)
		} else {
			knock.Super[0].Team2 = t
		}
	}
	if knock.Champions[len(knock.Champions)-1][0].Done {
		t, err := winner(knock.Champions[len(knock.Champions)-1][0])
		if err != nil {
			log.Println(err)
		} else {
			knock.Super[1].Team1 = t
		}
	}
}

// formMatchQueue creates a MatchQueue out of Gruppen
func formMatchQueue(gruppen Gruppen) *MatchQueue {
	q := new(MatchQueue)
	q.Waiting = make([][]Match, 6)

	pattern := [6][6]int{
		{1, 2, 5, 6, 3, 4},
		{3, 4, 1, 2, 5, 6},
		{5, 6, 3, 4, 1, 2},
		{1, 2, 5, 6, 3, 4},
		{3, 4, 1, 2, 5, 6},
		{5, 6, 3, 4, 1, 2},
	}
	for i := 0; i < len(pattern[0]); i++ {
		for j := 0; j < len(pattern); j++ {
			q.Waiting[pattern[j][i]-1] = append(q.Waiting[pattern[j][i]-1], gruppen[j][i])
		}
	}

	return q
}

// evaluate evaluates a slice of Matches and returns an object similar to Tabellen
func evaluate(matches []Match) ([4]Row, error) {
	table := new([4]Row)
	pattern := []int{0, 1, 2, 3, 0, 2, 1, 3, 3, 0, 1, 2}

	for i := 0; i < len(matches); i++ {
		pat1, pat2 := i*2, i*2+1

		//points
		p1, p2, err := points(matches[i])
		if err != nil {
			return [4]Row{}, err
		}
		table[pattern[pat1]].Punkte += p1
		table[pattern[pat2]].Punkte += p2
		//cups
		b1, b2 := cups(matches[i].Score1), cups(matches[i].Score2)
		table[pattern[pat1]].Becher += b1
		table[pattern[pat2]].Becher += b2
		//difference
		d1, d2 := cups(matches[i].Score1)-cups(matches[i].Score2), cups(matches[i].Score2)-cups(matches[i].Score1)
		table[pattern[pat1]].Differenz += d1
		table[pattern[pat2]].Differenz += d2
		//direct
		w, _ := winner(matches[i])
		table[pattern[pat1]].Vergleich[pattern[pat2]] = w.Name
		table[pattern[pat2]].Vergleich[pattern[pat1]] = w.Name
	}

	//name
	for i := 0; i < 2; i++ {
		table[pattern[i*2]].Team = matches[i].Team1
		table[pattern[i*2+1]].Team = matches[i].Team2
	}

	return *table, nil
}

// cups helps with negative values
func cups(n int) int {
	switch n {
	case -1:
		return 10
	case -2:
		return 16
	default:
		return n
	}
}

// isValid checks if a score is valid
func isValid(b1, b2 int) bool {
	if b1 == -1 && b2 >= 0 && b2 <= 10 {
		return true
	}
	if b2 == -1 && b1 >= 0 && b1 <= 10 {
		return true
	}
	if b1 == -2 && b2 >= 10 {
		return true
	}
	if b2 == -2 && b1 >= 10 {
		return true
	}
	if b1 == 10 && b2 < 10 {
		return true
	}
	if b2 == 10 && b1 < 10 {
		return true
	}
	if b1 == 16 && b2 >= 10 && b2 < 16 {
		return true
	}
	if b2 == 16 && b1 >= 10 && b1 < 16 {
		return true
	}
	if b1 == 19 && b2 >= 16 && b2 < 19 {
		return true
	}
	if b2 == 19 && b1 >= 16 && b1 < 19 {
		return true
	}
	if b1 >= 19 && b2 >= 19 && (b1 > b2 || b2 > b1) {
		return true
	}
	return false
}

// evalGruppen evaluates all groups in Gruppen and returns Tabellen
func evalGruppen(gruppen Gruppen) (Tabellen, error) {
	tabellen := make(Tabellen, len(gruppen))
	for i := 0; i < len(gruppen); i++ {
		t, err := evaluate(gruppen[i])
		if err != nil {
			return tabellen, err
		}
		tabellen[i] = t
	}
	sortTables(tabellen)
	return tabellen, nil
}

// HANDLERS //

var store = sessions.NewCookieStore([]byte(os.Getenv("SESSION_KEY")))

func checkSession(w http.ResponseWriter, r *http.Request) bool {
	// check if teams file exists
	if !fileExists(teamsPath) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return true
	}
	// get the session
	session, err := store.Get(r, "PongStrong")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	// check if session is new
	if session.IsNew {
		session.Options.MaxAge = -1
		log.Println("invalid session: ", session)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return true
	}
	return false
}

func killSession(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "PongStrong")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	session.Options.MaxAge = -1
	err = session.Save(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	log.Println("killed session: ", session)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {

		tmpl.ExecuteTemplate(w, "login.html", nil)
	} else if r.Method == "POST" {
		if r.FormValue("password") != os.Getenv("APP_PWD") {

			log.Println("incorrect password")
			tmpl.ExecuteTemplate(w, "login.html", "Versuche es erneut")
		} else {

			// get new session
			session, err := store.Get(r, "PongStrong")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			session.Options.MaxAge = 36000 // 10 hours
			// save session
			err = session.Save(r, w)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
		}
	} else {

		http.Error(w, "only GET and POST methods supported", http.StatusNotImplemented)
	}
}

type Config struct {
	Typ    byte
	Nummer int
}

func playingfieldHandler(w http.ResponseWriter, r *http.Request) {
	if checkSession(w, r) {
		return
	}
	if r.Method == "POST" {

		if r.FormValue("mode") == "start" {

			// put in function !!!
			id, err := strconv.Atoi(r.FormValue("table_id1"))
			if err != nil {

				log.Println(err)
				http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
			} else {

				matchQueue := ReadFromFile(new(MatchQueue), schlangePath)
				err := matchQueue.switchPlaying(id - 1)
				if err != nil {
					log.Println(err)
					http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
					return
				}
				WriteToFile(matchQueue, schlangePath)
				http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
			}

		} else if r.FormValue("mode") == "finnish" {

			table, err := strconv.Atoi(r.FormValue("table_id2"))
			if err != nil {

				log.Println(err)
			} else {

				// get and validate cup values
				cups1, err := strconv.Atoi(r.FormValue("cupsTeam1"))
				if err != nil {
					log.Println(err)
				}
				cups2, err := strconv.Atoi(r.FormValue("cupsTeam2"))
				if err != nil {
					log.Println(err)
				}
				if !isValid(cups1, cups2) {
					http.Error(w, "Überprüfe die Eingabe, die Becheranzahl ist innerhalb der Regeln nicht möglich", 400)
					return
				}
				// update MatchQueue and scores
				matchQueue := ReadFromFile(new(MatchQueue), schlangePath)
				id, err := matchQueue.getMatchID(table)
				if err != nil {
					log.Println(err)
					http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
					return
				}
				updateMatch(id, cups1, cups2, false)
				matchQueue.removeFromPlaying(table)
				matchQueue.updateKnockQueue()
				WriteToFile(matchQueue, schlangePath)
				http.Redirect(w, r, "/spielfeld", http.StatusSeeOther)
				// update tables
				groups := ReadFromFile(new(Gruppen), gruppenPath) // Optimierung möglich
				table, err := evalGruppen(*groups)
				if err != nil {
					log.Println(err)
				}
				WriteToFile(table, tabellenPath)
			}
		} else {

			http.Error(w, "mode not supported", http.StatusNotImplemented)
		}

	} else if r.Method == "GET" {

		matchQueue := ReadFromFile(new(MatchQueue), schlangePath)
		tables := ReadFromFile(new(Tabellen), tabellenPath)
		data := struct {
			Tabellen        Tabellen
			Matches         []Match
			NextMatches     []Match
			NextNextMatches []Match
		}{*tables, matchQueue.Playing, matchQueue.nextMatches(), matchQueue.NextNextMatches()}
		tmpl.ExecuteTemplate(w, "spielfeld.html", data)
	}
}

func overviewHandler(w http.ResponseWriter, r *http.Request) {
	if checkSession(w, r) {
		return
	}
	if r.Method == "GET" {
		// get Cookie
		session, err := store.Get(r, "PongStrong")
		if err != nil {
			log.Println("store.Get:" + err.Error())
		}
		// get selection
		selection := 0
		if n, ok := session.Values["groupNo"].(int); ok {
			selection = n
		}
		// construct html
		groups := ReadFromFile(new(Gruppen), gruppenPath)
		data := struct {
			Gruppen   Gruppen
			Selection int
		}{*groups, selection}
		tmpl.ExecuteTemplate(w, "übersicht.html", data)

	} else if r.Method == "POST" {
		// get form data
		groupNo, err := strconv.Atoi(r.FormValue("groupNo"))
		if err != nil {
			log.Println("strconv.Atoi:" + err.Error())
		}
		// get cookie
		sess, err := store.Get(r, "PongStrong")
		if err != nil {
			log.Println("store.Get:" + err.Error())
		}
		// store selection in Cookie
		sess.Values["groupNo"] = groupNo // "Same-site" atribut benötigt?
		sess.Save(r, w)

	} else {
		http.Error(w, "request not supported", http.StatusNotImplemented)

	}
}

func bracketHandler(w http.ResponseWriter, r *http.Request) {
	if checkSession(w, r) {
		return
	}
	knock := ReadFromFile(new(Knockouts), knockoutPath)
	tmpl.ExecuteTemplate(w, "turnierbaum.html", knock)
}

func rulesHandler(w http.ResponseWriter, r *http.Request) {
	if checkSession(w, r) {
		return
	}
	if r.Method == "GET" {
		tmpl.ExecuteTemplate(w, "regeln.html", nil)
	} else {
		http.Error(w, "only GET method supported", http.StatusNotImplemented)
	}
}

func controlpanelHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		tmpl.ExecuteTemplate(w, "controlpanel.html", nil)
	} else if r.Method == "POST" {

		// check if key is correct
		if r.FormValue("key") != os.Getenv("CTRLPANEL_PWD") {
			tmpl.ExecuteTemplate(w, "controlpanel.html", "key ist falsch")
			return
		}

		switch r.FormValue("type") {
		case "upload":
			err := saveFileFromForm(r, "gruppen")
			if err != nil {
				log.Println("saveFileFromForm:", err)
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Fehler beim Upload: "+err.Error()) // add check if file is .json / right format
				return
			} else {
				err := createTurnamentFiles()
				if err != nil {
					log.Println("createTurnamentFiles:", err)
					tmpl.ExecuteTemplate(w, "controlpanel.html", err.Error())
					return
				}
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Datei erfolgreich hochgeladen")
			}

		case "update":
			new1, _ := strconv.Atoi(r.FormValue("new1"))
			new2, _ := strconv.Atoi(r.FormValue("new2"))
			if !isValid(new1, new2) {
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Becheranzahl nicht möglich")
				return
			}
			err := updateMatch(r.FormValue("matchid"), new1, new2, true) // add true/false ignore checkmark?
			if err != nil {
				tmpl.ExecuteTemplate(w, "controlpanel.html", "updateMatch: "+err.Error())
			} else {
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Scores erfolgreich geändert")
				groups := ReadFromFile(new(Gruppen), gruppenPath) // Optimierung möglich
				table, err := evalGruppen(*groups)
				if err != nil {
					log.Println(err)
				}
				WriteToFile(table, tabellenPath)
			}

		case "evaluate":

			err := EvaluateGroups6()
			if err != nil {
				log.Println(err)
				tmpl.ExecuteTemplate(w, "controlpanel.html", "evaluateGroups: "+err.Error())

			} else {
				q := ReadFromFile(new(MatchQueue), schlangePath)
				if r.FormValue("ignorequeue") == "on" || q.isEmpty() {
					// update / create new Queue
					matchQueue := new(MatchQueue)
					matchQueue.Waiting = make([][]Match, 7) // table sensitive
					matchQueue.updateKnockQueue()
					WriteToFile(matchQueue, schlangePath)
					tmpl.ExecuteTemplate(w, "controlpanel.html", "Gruppenphase erforlgreich ausgewertet")

				} else {
					tmpl.ExecuteTemplate(w, "controlpanel.html", "Warteschlange ist nicht leer")

				}

			}

		case "reset":
			if os.Getenv("RESETABLE") == "true" && r.FormValue("confirmation") == "on" {
				err := resetTurnament(r.FormValue("keepgroups") == "on")
				if err != nil {
					tmpl.ExecuteTemplate(w, "controlpanel.html", "resetTurnament: "+err.Error())
					return
				}
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Turnier erfolgreich zurückgesetzt")
			} else {
				tmpl.ExecuteTemplate(w, "controlpanel.html", "Turnier nicht zurückgesetzt")
			}

		case "backup":
			files := []string{teamsPath, schlangePath, gruppenPath, knockoutPath}
			for _, p := range files {
				saveFileInCloud(p[3:]+"_backup", p)
			}

		default:
			http.Error(w, "form not supported", http.StatusNotImplemented)
		}

	} else {
		http.Error(w, "request not supported", http.StatusNotImplemented)
	}
}

// part of controlpanelHandler (saves a file from an http request to teamsPath)
func saveFileFromForm(r *http.Request, fileKey string) error {
	// get form file
	r.ParseMultipartForm(20)
	file, _, err := r.FormFile(fileKey)
	if err != nil {
		log.Println(err)
		return errors.New("r.FormFile: " + err.Error())
	}
	defer file.Close()
	// write to file
	fileBytes, err := io.ReadAll(file)
	if err != nil {
		log.Println(err)
		return errors.New("io.ReadAll: " + err.Error())
	}
	os.WriteFile(teamsPath, fileBytes, fs.ModePerm)
	// save teams.json in cloud
	saveFileInCloud(teamsCloudPath, teamsPath)
	return nil
}

// saveInCloud uploads a file to the pongstrong-backup-bucket
func saveFileInCloud(cloudPath, filepath string) error {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return errors.New("saveInCloud: storage.NewClient:" + err.Error())
	} else {
		err = uploadFile(client, ctx, cloudStorageBucket, cloudPath, filepath)
		if err != nil {
			return errors.New("saveInCloud: " + err.Error())
		}
		client.Close()
	}
	return nil
}

// part of controlpanelHandler (creates all necesary files in db from teams.json)
func createTurnamentFiles() error {
	// read in data
	if !fileExists(teamsPath) {
		return errors.New(teamsPath + " is missing")
	}
	teams := ReadFromFile(new(Teams), teamsPath)

	// create groups based on teams object and save it
	groups, err := CreateGruppen(*teams)
	if err != nil {
		return errors.New("CreateGruppen: " + err.Error())
	}
	WriteToFile(groups, gruppenPath)
	WriteToFile(formMatchQueue(groups), schlangePath)

	// create Knockouts object and save it
	knock := new(Knockouts)
	knock.Instatiate()
	WriteToFile(knock, knockoutPath)

	table, err := evalGruppen(groups)
	if err != nil {
		return errors.New("evalGruppen: " + err.Error())
	}
	WriteToFile(table, tabellenPath)

	return nil
}

// part of controlpanelHandler and playingfieldHandler (modifies gruppenPath, sets Match.Done to true)
func updateMatch(id string, score1, score2 int, done bool) error {
	switch id[0] {
	case 'g':
		groups := *ReadFromFile(new(Gruppen), gruppenPath)
		for i := 0; i < len(groups); i++ {
			for j := 0; j < len(groups[0]); j++ {
				if id == groups[i][j].ID && groups[i][j].Done == done {
					groups[i][j].Score1, groups[i][j].Score2 = score1, score2
					groups[i][j].Done = true
					WriteToFile(groups, gruppenPath)
					return nil
				}
			}
		}
		return errors.New("did not find match id:" + id)
	case 'c':
		knock := ReadFromFile(new(Knockouts), knockoutPath)
		for i := 0; i < 4; i++ {
			for j := 0; j < len(knock.Champions[i]); j++ {
				if id == knock.Champions[i][j].ID && knock.Champions[i][j].Done == done {
					knock.Champions[i][j].Score1, knock.Champions[i][j].Score2 = score1, score2
					knock.Champions[i][j].Done = true
					knock.update()
					WriteToFile(knock, knockoutPath)
					return nil
				}
			}
		}
		return errors.New("did not find match id:" + id)
	case 'e':
		knock := ReadFromFile(new(Knockouts), knockoutPath)
		for i := 0; i < 3; i++ {
			for j := 0; j < len(knock.Europa[i]); j++ {
				if id == knock.Europa[i][j].ID && knock.Europa[i][j].Done == done {
					knock.Europa[i][j].Score1, knock.Europa[i][j].Score2 = score1, score2
					knock.Europa[i][j].Done = true
					knock.update()
					WriteToFile(knock, knockoutPath)
					return nil
				}
			}
		}
		return errors.New("did not find match id:" + id)
	case 'f':
		knock := ReadFromFile(new(Knockouts), knockoutPath)
		for i := 0; i < 3; i++ {
			for j := 0; j < len(knock.Conference[i]); j++ {
				if id == knock.Conference[i][j].ID && knock.Conference[i][j].Done == done {
					knock.Conference[i][j].Score1, knock.Conference[i][j].Score2 = score1, score2
					knock.Conference[i][j].Done = true
					knock.update()
					WriteToFile(knock, knockoutPath)
					return nil
				}
			}
		}
		return errors.New("did not find match id:" + id)
	case 's':
		knock := ReadFromFile(new(Knockouts), knockoutPath)
		for i := 0; i < 2; i++ {
			if knock.Super[i].ID == id && knock.Super[i].Done == done {
				knock.Super[i].Score1, knock.Super[i].Score2 = score1, score2
				knock.Super[i].Done = true
				knock.update()
				WriteToFile(knock, knockoutPath)
				return nil
			}
		}
		return errors.New("did not find match id:" + id)
	default:
		return errors.New("id is invalid")
	}
}

// part of controlpanelHandler (switches the turnament into knockout mode)
func EvaluateGroups8() error {
	table := *ReadFromFile(new(Tabellen), tabellenPath)
	sortTables(table)

	teams := make([][]Team, len(table))
	for i := 0; i < len(table); i++ {
		teams[i] = make([]Team, 4)
		for j := 0; j < 4; j++ {
			teams[i][j] = table[i][j].Team
		}
	}

	// save knockout startconfig
	knock := new(Knockouts)
	knock.Instatiate()

	// champ
	fir := [8]int{0, 2, 4, 6, 1, 3, 5, 7}
	sec := [8]int{1, 3, 5, 7, 0, 2, 4, 6}
	for j := 0; j < 8; j++ {
		knock.Champions[0][j].Team1 = teams[fir[j]][0]
		knock.Champions[0][j].Team2 = teams[sec[j]][1]
	}

	// euro & conf
	i, j := 0, 0
	for j < 8 {
		if j%2 == 0 {
			knock.Europa[0][i].Team1 = teams[j][2]
			knock.Conference[0][i].Team1 = teams[j][3]
		} else {
			knock.Europa[0][i].Team2 = teams[j][2]
			knock.Conference[0][i].Team2 = teams[j][3]
			i++
		}
		j++
	}

	mapTables(knock)
	WriteToFile(knock, knockoutPath)

	return nil
}

// part of controlpanelHandler (switches the turnament into knockout mode)
func EvaluateGroups6() error {
	table := *ReadFromFile(new(Tabellen), tabellenPath)
	sortTables(table)

	teams := make([][]Team, len(table))
	for i := 0; i < len(table); i++ {
		teams[i] = make([]Team, 4)
		for j := 0; j < 4; j++ {
			teams[i][j] = table[i][j].Team
		}
	}

	// save startconfig
	knock := new(Knockouts)
	knock.Instatiate()

	// CHAMP
	first := [6]int{1, 4, 2, 5, 3, 6}
	for j := 0; j < 6; j++ {
		knock.Champions[0][first[j]].Team1 = teams[j][0]
	}

	second := [6][2]int{{7, 0}, {0, 0}, {5, 1}, {2, 1}, {7, 1}, {0, 1}}
	for j := 0; j < 6; j++ {
		if second[j][1] == 0 {
			knock.Champions[0][second[j][0]].Team1 = teams[j][1]
		} else {
			knock.Champions[0][second[j][0]].Team2 = teams[j][1]
		}
	}

	// finde beste dritte
	allThirds := make([]Row, 6)
	for i := 0; i < 6; i++ {
		allThirds[i] = table[i][2]
	}
	SortTable(allThirds)

	thirds := make([]Team, 6)
	for i := 0; i < 6; i++ {
		thirds[i] = allThirds[i].Team
	}
	bestThirds := thirds[:4]

	pattern := [][][]int{{{1}, {2, 3, 4}}, {{3}, {0, 1, 5}}, {{4}, {0, 4, 5}}, {{6}, {1, 2, 3}}}
	// befülle restplätze 1,3,4,6 mit besten dritten
	for i := 0; i < 3; i++ {

		bt := make([]Team, 4)
		copy(bt, bestThirds)

		for i := 0; i < 4; i++ {
		loop:
			for j := 0; j < len(bt); j++ {
				org := bt[j].origin()
				for k := 0; k < 3; k++ {
					if pattern[i][1][k] == org {
						knock.Champions[0][pattern[i][0][0]].Team2 = bt[j] // add team to knock
						bt = append(bt[:j], bt[j+1:]...)                   // remove team from bt
						break loop                                         // skip to next hole in knock
					}
				}
			}
		}

		if len(bt) == 0 {
			break
		}
		bestThirds = append(bestThirds[3:4], bestThirds[:3]...)
	}

	// EUROPA
	// finde beste vierte
	allFourth := make([]Row, 6)
	for i := 0; i < 6; i++ {
		allFourth[i] = table[i][3]
	}
	SortTable(allFourth)
	fourth := make([]Team, 6)
	for i := 0; i < 6; i++ {
		fourth[i] = allFourth[i].Team
	}

	euroTeams := thirds[4:]
	euroTeams = append(euroTeams, fourth[:2]...)
	knock.Europa[1][0].Team1 = euroTeams[0]
	knock.Europa[1][0].Team2 = euroTeams[1]
	knock.Europa[1][1].Team1 = euroTeams[2]
	knock.Europa[1][1].Team2 = euroTeams[3]

	// CONF
	confTeams := fourth[2:]
	knock.Conference[1][0].Team1 = confTeams[0]
	knock.Conference[1][0].Team2 = confTeams[1]
	knock.Conference[1][1].Team1 = confTeams[2]
	knock.Conference[1][1].Team2 = confTeams[3]

	mapTables(knock)
	WriteToFile(knock, knockoutPath)

	return nil
}

// mapTables maps the tables to the Knockout matches
func mapTables(knock *Knockouts) {
	greenprint := [][]int{{1, 2, 3, 4}, {5}, {6}}
	periIndices := func(i *int, i_max int) int {
		if *i == i_max-1 {
			*i = 0
			return i_max - 1
		}
		*i++
		return *i - 1
	}
	ip := 0

	// champ
	for i := 0; i < 8; i++ {
		knock.Champions[0][i].TischNr = greenprint[0][periIndices(&ip, len(greenprint[0]))]
	}
	ip = len(greenprint[0]) / 2
	for i := 0; i < 4; i++ {
		knock.Champions[1][i].TischNr = greenprint[0][periIndices(&ip, len(greenprint[0]))]
	}
	ip = 0
	for i := 0; i < 2; i++ {
		knock.Champions[2][i].TischNr = greenprint[0][periIndices(&ip, len(greenprint[0]))]
	}
	knock.Champions[3][0].TischNr = greenprint[0][0]
	// euro
	ip = 0
	for i := 0; i < 4; i++ {
		knock.Europa[0][i].TischNr = greenprint[1][periIndices(&ip, len(greenprint[1]))]
	}
	ip = len(greenprint[1]) / 2
	for i := 0; i < 2; i++ {
		knock.Europa[1][i].TischNr = greenprint[1][periIndices(&ip, len(greenprint[1]))]
	}
	knock.Europa[2][0].TischNr = greenprint[1][0]
	// conf
	ip = 0
	for i := 0; i < 4; i++ {
		knock.Conference[0][i].TischNr = greenprint[2][periIndices(&ip, len(greenprint[2]))]
	}
	ip = len(greenprint[1]) / 2
	for i := 0; i < 2; i++ {
		knock.Conference[1][i].TischNr = greenprint[2][periIndices(&ip, len(greenprint[2]))]
	}
	knock.Conference[2][0].TischNr = greenprint[2][0]
	// super
	knock.Super[0].TischNr = 1
	knock.Super[1].TischNr = 1
}

// part of controlpanelHandler (deletes all db files)
func resetTurnament(keepTeams bool) error {
	// remove saved data
	paths := []string{gruppenPath, knockoutPath, tabellenPath, schlangePath}
	for _, p := range paths {
		err := os.Remove(p)
		if err != nil {
			log.Println("os.Remove: ", err)
			return errors.New("error beim Löschen der Turnierdaten: " + p)
		}
	}
	// remove groups or start new turnament
	if keepTeams {
		createTurnamentFiles()
	} else {
		err := os.Remove(teamsPath)
		if err != nil {
			log.Println("os.Remove: ", err)
			return errors.New("error beim Löschen der Turnierdaten: " + gruppenPath)
		}
	}
	return nil
}

// ---- Geplante Features ----
// - cookies PopUp -> "Oma zwingt dich ihre kekse zu essen"
// - Match bereits abgeschlossen fehlermeldung
// - manuelles backup zusätzlich zu cloud state recovery
// - support für sehr lange Namen
// - Heim / Auswärts sichtbar
// - deathcup Anzeige vereinfachen
// - bessere performance
// - browser support
//
// --> make unused games grey??
func main() {
	log.Println("started new container")

	// new servermultiplexer
	mux := http.NewServeMux()
	tmpl = template.Must(template.ParseGlob("templates/*.html"))

	// start fileserver
	os.Mkdir("db", fs.ModeDir)
	mux.Handle("/db/", noDirListing(http.FileServer(http.Dir("db"))))
	mux.Handle("/templates/", noDirListing(http.FileServer(http.Dir("templates"))))

	// create a new client for cloud storage access
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		log.Println("storage.NewClient:", err)
	} else {
		// get saved data from cloud
		files := []string{teamsPath, schlangePath, gruppenPath, knockoutPath}
		objects := []string{teamsCloudPath, schlangeCloudPath, gruppenCloudPath, knockoutCloudPath}
		for i := 0; i < 4; i++ {
			err = downloadObject(client, ctx, cloudStorageBucket, objects[i], files[i])
			if err != nil {
				log.Printf("downloadObject: %s: %s", objects[i], err)
			}
		}

		// create table
		groups := ReadFromFile(new(Gruppen), gruppenPath)
		table, err := evalGruppen(*groups)
		if err != nil {
			log.Println(err)
		}
		WriteToFile(table, tabellenPath)
	}
	defer client.Close()

	// ACHTUNG WIEDER ENTFERNEN? //
	groups := ReadFromFile(new(Gruppen), gruppenPath)
	table, err := evalGruppen(*groups)
	if err != nil {
		log.Println(err)
	}
	WriteToFile(table, tabellenPath)
	///////////////////////////////

	savePoints()

	// add functions to paths
	mux.HandleFunc("/controlpanel", controlpanelHandler)
	mux.HandleFunc("/spielfeld", playingfieldHandler)
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/killsession", killSession)
	mux.HandleFunc("/übersicht", overviewHandler)
	mux.HandleFunc("/turnierbaum", bracketHandler)
	mux.HandleFunc("/regeln", rulesHandler)

	go func(ctx context.Context, client *storage.Client) {
		if client == nil {
			log.Println("no client found: exiting save loop")
			return
		}

		// read in backup time from env
		btime, err := strconv.Atoi(os.Getenv("BACKUP_TIME"))
		if err != nil || btime == 0 {
			btime = 3 // minutes
		}

		for {
			// save local files in cloud
			files := []string{schlangePath, gruppenPath, knockoutPath}
			objects := []string{schlangeCloudPath, gruppenCloudPath, knockoutCloudPath}
			for i := 0; i < 3; i++ {
				if fileExists(files[i]) {
					err = uploadFile(client, ctx, cloudStorageBucket, objects[i], files[i])
					if err != nil {
						log.Printf("uploadObject: %s: %s", objects[i], err)
					}
				}
			}

			// sleep
			for i := 0; i < btime; i++ {
				time.Sleep(time.Minute)
			}
		}
	}(ctx, client)

	// determine port for HTTP and start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
		log.Printf("defaulting to port %s", port)
	}

	// start server
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// noDirListing makes the filesystem inaccessable through direct http requests
func noDirListing(h http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// fileExists returns true if the file exists
func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

// uploadFile uploads an object to a Cloud Storage bucket (cloud storage)
func uploadFile(client *storage.Client, ctx context.Context, bucket, object, filepath string) error {

	// open local file
	f, err := os.Open(filepath)
	if err != nil {
		return fmt.Errorf("os.Open: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(ctx, time.Second*50)
	defer cancel()

	// get an ObjectHandle
	obj := client.Bucket(bucket).Object(object)
	//obj = obj.If(storage.Conditions{DoesNotExist: true})

	// upload an object with storage.Writer
	sw := obj.NewWriter(ctx)
	if _, err = io.Copy(sw, f); err != nil {
		return fmt.Errorf("io.Copy: %v", err)
	}
	if err := sw.Close(); err != nil {
		return fmt.Errorf("Writer.Close: %v", err)
	}

	log.Println("uploaded", filepath, "to", bucket)
	return nil
}

// downloadObject downloads an object to a file (cloud storage)
func downloadObject(client *storage.Client, ctx context.Context, bucket, object string, destFileName string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second*50)
	defer cancel()

	f, err := os.Create(destFileName)
	if err != nil {
		return fmt.Errorf("os.Create: %v", err)
	}

	sr, err := client.Bucket(bucket).Object(object).NewReader(ctx)
	if err != nil {
		return fmt.Errorf("Object(%q).NewReader: %v", object, err)
	}
	defer sr.Close()

	if _, err := io.Copy(f, sr); err != nil {
		return fmt.Errorf("io.Copy: %v", err)
	}

	if err = f.Close(); err != nil {
		return fmt.Errorf("f.Close: %v", err)
	}

	log.Println("downloaded", object, "to local file", destFileName)
	return nil
}

// Calculate Legacy Points //

func listTeams() []Team {
	teams := ReadFromFile(new(Teams), teamsPath)
	list := make([]Team, len(*teams)*4)

	k := 0
	for _, v := range *teams {
		for i := 0; i < 4; i++ {
			list[k] = v[i]
			k++
		}
	}
	return list
}

type playerPoints struct {
	Name   string `json:"name"`
	Points int    `json:"points"`
}

func savePoints() {
	teams := listTeams()
	points := make([]int, len(teams))
	table := *ReadFromFile(new(Tabellen), tabellenPath)
	knock := ReadFromFile(new(Knockouts), knockoutPath)

	for n, team := range teams {
		// Teilnahme
		points[n] = 1

		// Gruppenphase Platzierung
		for i := 0; i < len(table); i++ {
			for j := 0; j < 3; j++ {
				if team == table[i][j].Team {
					switch j {
					case 0:
						points[n] = 4
					case 1:
						points[n] = 3
					case 2:
						points[n] = 2
					}
				}
			}
		}

		// Knockouts Platzierung
		for i := 1; i < 4; i++ {
			for j := 0; j < len(knock.Champions[i]); j++ {
				if knock.Champions[i][j].Team1 == team || knock.Champions[i][j].Team2 == team {
					switch i {
					case 1:
						points[n] = 6
					case 2:
						points[n] = 8
					case 3:
						win, err := winner(knock.Champions[i][j])
						if err != nil {
							log.Println(err)
						}
						if win == team {
							points[n] = 12
						} else {
							points[n] = 10
						}
					}
				}
			}
		}
		// Euro
		for i := 1; i < 3; i++ {
			for j := 0; j < len(knock.Europa[i]); j++ {
				if knock.Europa[i][j].Team1 == team || knock.Europa[i][j].Team2 == team {
					switch i {
					case 1:
						points[n] = 2
					case 2:
						win, err := winner(knock.Europa[i][j])
						if err != nil {
							log.Println(err)
						}
						if win == team {
							points[n] = 4
						} else {
							points[n] = 3
						}
					}
				}
			}
		}
		// Conf
		if knock.Conference[2][0].Team1 == team || knock.Conference[2][0].Team2 == team {
			win, err := winner(knock.Conference[2][0])
			if err != nil {
				log.Println(err)
			}
			if win == team {
				points[n] = 3
			} else {
				points[n] = 2
			}
		}
		// Super
		superPoints := 0
		if knock.Super[0].Team1 == team || knock.Super[0].Team2 == team {
			win, err := winner(knock.Super[0])
			if err != nil {
				log.Println(err)
			}
			if win == team {
				superPoints = 1
			}
		}
		if knock.Super[1].Team1 == team || knock.Super[1].Team2 == team {
			win, err := winner(knock.Super[1])
			if err != nil {
				log.Println(err)
			}
			if win == team {
				superPoints = 2
			}
		}
		points[n] += superPoints
	}

	playerPoints := make([]playerPoints, len(points)*2)
	for k, team := range teams {
		playerPoints[2*k].Name = team.Mem1
		playerPoints[2*k].Points = points[k]
		playerPoints[2*k+1].Name = team.Mem2
		playerPoints[2*k+1].Points = points[k]
	}

	WriteToFile(playerPoints, "SpielerPunkte.json")
}
