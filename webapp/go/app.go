package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	goCache "github.com/pmylund/go-cache"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var db *sqlx.DB
var tmpl *template.Template
var port = flag.Uint("port", 0, "port to listen")
var appDir = flag.String("appdir", ".", "the directory where public & views directories are located")
var mysqlSock = flag.String("mysqlsock", "", "mysql unix socket path")
var SeatMapCacheOf = make(map[uint]SeatMapCache, 100)
var gocache = goCache.New(30*time.Second, 10*time.Second)

var ArtistNames = []string{"", "NHN48", "はだいろクローバーZ"}
var TicketNames = []string{"", "西武ドームライブ", "東京ドームライブ", "さいたまスーパーアリーナライブ", "横浜アリーナライブ", "西武ドームライブ"}
var VariationNames = []string{"", "アリーナ席", "スタンド席", "アリーナ席", "スタンド席", "アリーナ席", "スタンド席", "アリーナ席", "スタンド席", "アリーナ席", "スタンド席"}

var RecentSoldList = make([]RecentSold, 10)
var RecentSoldListLen = 0

var rsMutex *sync.RWMutex

func main() {
	flag.Parse()
	connectDB()
	initTmpl()

	rsMutex = &sync.RWMutex{}

	serveHTTP()
}

func init() {
}

func getAppDir() string {
	return *appDir
}

func serveHTTP() {
	r := mux.NewRouter()
	r.HandleFunc("/", indexHandler).Methods("GET")
	r.HandleFunc("/artist/{artistid:[0-9]+}", artistHandler).Methods("GET")
	r.HandleFunc("/ticket/{ticketid:[0-9]+}", ticketHandler).Methods("GET")
	r.HandleFunc("/buy", buyHandler).Methods("POST")
	r.HandleFunc("/admin", adminHandler).Methods("GET")
	r.HandleFunc("/admin/order.csv", adminOrderHandler).Methods("GET")
	r.HandleFunc("/admin", adminPostHandler).Methods("POST")
	r.PathPrefix("/").Handler(http.FileServer(http.Dir(getAppDir() + "/public/")))
	http.Handle("/", r)

	sigchan := make(chan os.Signal)
	signal.Notify(sigchan, os.Interrupt)
	signal.Notify(sigchan, syscall.SIGTERM)
	signal.Notify(sigchan, syscall.SIGINT)

	var l net.Listener
	var err error
	if *port == 0 {
		ferr := os.Remove("/tmp/server.sock")
		if ferr != nil {
			if !os.IsNotExist(ferr) {
				panic(ferr.Error())
			}
		}
		l, err = net.Listen("unix", "/tmp/server.sock")
		os.Chmod("/tmp/server.sock", 0777)
	} else {
		l, err = net.ListenTCP("tcp", &net.TCPAddr{Port: int(*port)})
	}
	if err != nil {
		panic(err.Error())
	}
	go func() {
		log.Println(http.Serve(l, nil))
	}()

	<-sigchan
}

type dbConfig struct {
	Host     string
	Port     uint
	Username string
	Password string
	DBname   string
}

type Config struct {
	Database dbConfig
}

type Artist struct {
	ID   uint
	Name string
}

type Ticket struct {
	ID       uint
	Name     string
	ArtistID uint `db:"artist_id"`
}

type TicketWithCount struct {
	Ticket
	Count int
}

type TicketWithArtist struct {
	Ticket
	ArtistName string `db:"artist_name"`
}

type Variation struct {
	ID       uint
	Name     string
	TicketID uint `db:"ticket_id"`
}

type VariationWithStocks struct {
	Variation
	Count  uint
	Stocks [64][64]Stock
}

type Stock struct {
	ID          uint
	VariationID uint          `db:"variation_id"`
	SeatID      string        `db:"seat_id"`
	OrderID     sql.NullInt64 `db:"order_id"`
	UpdatedAt   string        `db:"updated_at"`
}

type OrderRequest struct {
	ID       uint
	MemberID string `db:"member_id"`
}

type RecentSold struct {
	SeatID string `db:"seat_id"`
	VName  string `db:"v_name"`
	TName  string `db:"t_name"`
	AName  string `db:"a_name"`
}

type OrderRequestCSV struct {
	OrderRequest
	VariationID uint   `db:"variation_id"`
	SeatID      string `db:"seat_id"`
	UpdatedAt   string `db:"updated_at"`
}

type SeatMapCache struct {
	VariationID uint
	Content     template.HTML
	ExpireAt    int64
}

func (csv OrderRequestCSV) ToLine() string {
	return fmt.Sprintf("%d,%s,%s,%d,%s\n", csv.ID, csv.MemberID, csv.SeatID, csv.VariationID, csv.UpdatedAt)
}

func getConfig() Config {
	f, err := os.Open(getAppDir() + "/../config/common.local.json")
	if err != nil {
		panic(err.Error())
	}
	var conf Config
	dec := json.NewDecoder(f)
	err = dec.Decode(&conf)
	if err != nil {
		panic(err.Error())
	}
	return conf
}

func connectDB() {
	var err error
	conf := getConfig()
	dbConf := conf.Database

	var dsn string
	if *mysqlSock != "" {
		dsn = fmt.Sprintf("%s:%s@unix(%s)/%s",
			dbConf.Username, dbConf.Password, *mysqlSock, dbConf.DBname)
	} else {
		dsn = fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
			dbConf.Username, dbConf.Password, dbConf.Host, dbConf.Port, dbConf.DBname)
	}

	db, err = sqlx.Open("mysql", dsn)
	err = db.Ping()
	if err != nil {
		panic(err.Error())
	}
}

func initTmpl() {
	tmpl = template.Must(template.ParseGlob(getAppDir() + "/views/*.html"))
}

func getRecentSold() ([]RecentSold, error) {
	rsMutex.RLock()
	recents := make([]RecentSold, RecentSoldListLen)
	for i := 0; i < RecentSoldListLen; i++ {
		recents[i] = RecentSoldList[i]
	}
	rsMutex.RUnlock()
	//log.Printf("%#v", recents)
	return recents, nil
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	artists := []Artist{}
	err := db.Select(&artists, "SELECT * FROM artist ORDER BY id")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	recents, err := getRecentSold()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("%#v", recents)

	tmpl.ExecuteTemplate(w, "index.html", map[string]interface{}{
		"recents": recents,
		"artists": artists,
	})
}

func artistHandler(w http.ResponseWriter, r *http.Request) {
	artistid := mux.Vars(r)["artistid"]

	artist := Artist{}
	err := db.Get(&artist, "SELECT id, name FROM artist WHERE id = ? LIMIT 1", artistid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("%#v", artist)

	tickets := []TicketWithCount{}
	err = db.Select(&tickets, "SELECT id, name FROM ticket WHERE artist_id = ? ORDER BY id", artistid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	for i := 0; i < len(tickets); i++ {
		ticket := &tickets[i]
		key := "count_" + strconv.FormatInt(int64(i), 10)
		if x, found := gocache.Get(key); found {
			ticket.Count = x.(int)
		} else {
			err = db.Get(&ticket.Count, `
SELECT COUNT(*) AS cnt FROM variation
INNER JOIN stock ON stock.variation_id = variation.id
WHERE variation.ticket_id = ? AND stock.order_id IS NULL`, ticket.ID)
			gocache.Set(key, ticket.Count, 0)
			//log.Printf("%+v", ticket.Count)
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	//log.Printf("%#v", tickets)

	recents, err := getRecentSold()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	tmpl.ExecuteTemplate(w, "artist.html", map[string]interface{}{
		"recents": recents,
		"artist":  artist,
		"tickets": tickets,
	})
}

func ticketHandler(w http.ResponseWriter, r *http.Request) {
	ticketid := mux.Vars(r)["ticketid"]

	ticket := TicketWithArtist{}
	err := db.Get(&ticket, `
SELECT t.*, a.name AS artist_name FROM ticket t
INNER JOIN artist a ON t.artist_id = a.id
WHERE t.id = ? LIMIT 1`, ticketid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("%+v", ticket)

	variations := []VariationWithStocks{}
	err = db.Select(&variations,
		`SELECT id, name FROM variation WHERE ticket_id = ? ORDER BY id`, ticketid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("%+v", variations)

	for i := 0; i < len(variations); i++ {
		v := &variations[i]
		err := db.Get(&v.Count,
			`SELECT COUNT(*) AS cnt FROM stock WHERE variation_id = ? AND order_id IS NULL`, v.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		//log.Printf("%+v", v.Count)

		stocks := []Stock{}
		err = db.Select(&stocks,
			`SELECT seat_id, order_id FROM stock WHERE variation_id = ?`, v.ID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		//log.Printf("%+v", stocks)
		for _, stock := range stocks {
			if stock.OrderID.Valid {
				//log.Printf("%+v", stock)
			}
			m := regexp.MustCompile(`(\d+)-(\d+)`).FindStringSubmatch(stock.SeatID)
			row, _ := strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			v.Stocks[row][col] = stock
		}
	}

	recents, err := getRecentSold()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	seatMaps := make([]template.HTML, len(variations))
	for i, variation := range variations {
		cache, ok := SeatMapCacheOf[variation.ID]
		if ok && cache.ExpireAt >= time.Now().Unix() {
			seatMaps[i] = cache.Content
		} else {
			cache := generateSeatMapCache(variation)
			seatMaps[i] = cache.Content
		}
	}

	tmpl.ExecuteTemplate(w, "ticket.html", map[string]interface{}{
		"recents":    recents,
		"ticket":     ticket,
		"seatMaps":   seatMaps,
		"variations": variations,
	})
}

func generateSeatMapCache(variation VariationWithStocks) SeatMapCache {
	fmt.Println("expired!!!")
	var doc bytes.Buffer
	tmpl.ExecuteTemplate(&doc, "zaseki.html", map[string]interface{}{
		"variation": variation,
	})
	var cache SeatMapCache
	cache.VariationID = variation.ID
	cache.Content = template.HTML(doc.String())
	cache.ExpireAt = time.Now().Unix() + 10
	SeatMapCacheOf[variation.ID] = cache
	return cache
}

func buyHandler(w http.ResponseWriter, r *http.Request) {
	memberid := r.PostFormValue("member_id")
	variationid := r.PostFormValue("variation_id")
	//log.Printf("memberid: %s, variationid: %s", memberid, variationid)
	variationId, err := strconv.Atoi(variationid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	tx, err := db.Beginx()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	res, err := tx.Exec("INSERT INTO order_request (member_id) VALUES (?)", memberid)
	if err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), 500)
		return
	}

	orderid, err := res.LastInsertId()
	if err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("orderid: %d", orderid)

	res, err = tx.Exec(`
UPDATE stock SET order_id = ?
	WHERE variation_id = ? AND order_id IS NULL
	ORDER BY RAND() LIMIT 1`, orderid, variationid)
	if err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), 500)
		return
	}

	aff, err := res.RowsAffected()
	if err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), 500)
		return
	}
	//log.Printf("affected: %d", aff)
	if aff == 0 {
		tx.Rollback()
		tmpl.ExecuteTemplate(w, "soldout.html", nil)
		return
	}

	var seatid string
	err = tx.Get(&seatid, "SELECT seat_id FROM stock WHERE order_id = ? LIMIT 1", orderid)
	if err != nil {
		tx.Rollback()
		http.Error(w, err.Error(), 500)
		return
	}
	tx.Commit()

	v, ok := SeatMapCacheOf[uint(variationId)]
	if ok {
		v.ExpireAt = 0
	}

	vid, _ := strconv.Atoi(variationid)
	vid = vid
	tid := (vid + 1) / 2
	aid := 1
	if tid >= 3 {
		aid = 2
	}
	//log.Printf("%d %s, %d %s, %d %s", vid, VariationNames[vid], tid, TicketNames[tid], aid, ArtistNames[aid])

	rsMutex.Lock()
	for i := RecentSoldListLen; i > 0; i-- {
		if i == 10 {
			continue
		}
		RecentSoldList[i] = RecentSoldList[i-1]
	}
	RecentSoldList[0] = RecentSold{
		SeatID: seatid,
		VName:  VariationNames[vid],
		TName:  TicketNames[tid],
		AName:  ArtistNames[aid],
	}
	if RecentSoldListLen < 10 {
		RecentSoldListLen++
	}
	rsMutex.Unlock()

	tmpl.ExecuteTemplate(w, "complete.html", map[string]interface{}{
		"seatid":   seatid,
		"memberid": memberid,
	})
}

func adminHandler(w http.ResponseWriter, r *http.Request) {
	tmpl.ExecuteTemplate(w, "admin.html", nil)
}

func adminOrderHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Queryx(`
SELECT order_request.*, stock.seat_id, stock.variation_id, stock.updated_at
FROM order_request JOIN stock ON order_request.id = stock.order_id
ORDER BY order_request.id ASC`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	var body bytes.Buffer

	for rows.Next() {
		var csv OrderRequestCSV
		err = rows.StructScan(&csv)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		body.WriteString(csv.ToLine())
	}
	rows.Close()

	w.Header().Set("Content-Type", "text/csv")
	_, _ = body.WriteTo(w)
}

func adminPostHandler(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(getAppDir() + "/../config/database/initial_data.sql")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	b := bufio.NewReader(f)

	for {
		line, err := b.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			http.Error(w, err.Error(), 500)
			return
		}

		l := strings.TrimSpace(line)
		if len(l) == 0 {
			continue
		}
		_, err = db.Exec(l)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	rsMutex.Lock()
	RecentSoldListLen = 0
	rsMutex.Unlock()

	http.Redirect(w, r, "/admin", 302)
}
