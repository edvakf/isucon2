package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	goCache "github.com/pmylund/go-cache"
	"github.com/garyburd/redigo/redis"
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
var buyTicketQueue = make(chan BuyTicketTask)

func main() {
	flag.Parse()
	connectDB()
	initTmpl()

	go ticketGenerator()
	rsMutex = &sync.RWMutex{}

	serveHTTP()

	db.Close()
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
}

type BuyTicketResult struct {
	SeatId string
	Error error
}

type BuyTicketTask struct {
	StockId int
	OrderId int64
	Result chan BuyTicketResult
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

func connectRedis() (redis.Conn, error) {
	c, err := redis.Dial("tcp", ":6379")
	return c, err
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

	cacheKey := fmt.Sprintf("ticket_page%s", ticketid)
	v, found := gocache.Get(cacheKey)
	var result string
	var err error
	if found {
		result = v.(string)
	} else {
		fmt.Println("expired !!!")

		http.Error(w, errors.New("expired").Error(), 500)
		return

		result, err = renderTicketTemplate(ticketid)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	w.Write([]byte(result))
}

func generateSeatMapCache(variation VariationWithStocks) SeatMapCache {
	var doc bytes.Buffer
	seatRange := [64]int{}
	seatRange2 := [64]int{}
	for i := 0; i < 64; i++ {
		seatRange[i] = i
		seatRange2[i] = i
	}

	c, err := connectRedis()
	if err != nil {
		panic(err)
	}
	vals, err := redis.Strings(c.Do("SMEMBERS", fmt.Sprintf("stock_%d", variation.ID)))
	if err != nil {
		panic(err)
	}

	stockOf := make(map[string]bool)

	for _, v := range vals {
		stockOf[v] = true
	}

	var seatMap [64][64]bool
	for i := 0; i < 64; i++ {
		for j := 0; j < 64; j++ {
			_, ok := stockOf[fmt.Sprintf("%02d-%02d", i, j)]
			seatMap[i][j] = ok
		}
	}

	tmpl.ExecuteTemplate(&doc, "zaseki.html", map[string]interface{}{
		"variation": variation,
		"seatMap": seatMap,
	})
	var cache SeatMapCache
	cache.VariationID = variation.ID
	cache.Content = template.HTML(doc.String())
	SeatMapCacheOf[variation.ID] = cache
	return cache
}

func buyHandler(w http.ResponseWriter, r *http.Request) {
	memberid := r.PostFormValue("member_id")
	variationid := r.PostFormValue("variation_id")
	//log.Printf("memberid: %s, variationid: %s", memberid, variationid)

	c, err := connectRedis()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	orderid, err := redis.Int(c.Do("INCR", "order_request_id"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	seatid, err := redis.String(c.Do("SPOP", fmt.Sprintf("stock_%s", variationid)))
	if err != nil {
		if err == redis.ErrNil {
			tmpl.ExecuteTemplate(w, "soldout.html", nil)
			return
		} else {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	now := time.Now()
	var columns []string
	columns = append(columns, fmt.Sprintf("%d", orderid))
	columns = append(columns, memberid)
	columns = append(columns, seatid)
	columns = append(columns, variationid)
	columns = append(columns, now.Format("2006-01-02 15:04:05"))
	c.Do("RPUSH", "order_request", strings.Join(columns, ","))

	// go func(variationid string) {
	// 	key := fmt.Sprintf("seat_map_cahce_of_%d", variationid)
	// 	gocache.Delete(key)
	// }(variationid)


	go func(variationid string) {
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
	}(variationid)

	tmpl.ExecuteTemplate(w, "complete.html", map[string]interface{}{
		"seatid":   seatid,
		"memberid": memberid,
	})
}

func adminHandler(w http.ResponseWriter, r *http.Request) {
	tmpl.ExecuteTemplate(w, "admin.html", nil)
}

func adminOrderHandler(w http.ResponseWriter, r *http.Request) {
	c, err := connectRedis()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	lines, err := redis.Strings(c.Do("LRANGE", "order_request", 0, -1))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var body bytes.Buffer
	for _, line := range lines {
		body.WriteString(line + "\n")
	}

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

	c, err := connectRedis()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_, err = c.Do("FLUSHDB")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	stocks := []Stock{}
	err = db.Select(&stocks, "SELECT * FROM stock")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return;
	}

	c.Send("MULTI")
	for _, stock := range stocks {
		err = c.Send("SADD", fmt.Sprintf("stock_%d", stock.VariationID), stock.SeatID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	_, err = c.Do("EXEC")
	if err != nil {
			http.Error(w, err.Error(), 500)
			return
	}

	rsMutex.Lock()
	RecentSoldListLen = 0
	rsMutex.Unlock()

	http.Redirect(w, r, "/admin", 302)
}

func loadStocksToVariation(v *VariationWithStocks) (error) {
	//log.Printf("%+v", v.Count)

	stocks := []Stock{}
	err := db.Select(&stocks,
		`SELECT seat_id, order_id FROM stock WHERE variation_id = ?`, v.ID)
	if err != nil {
		return err
	}
	//log.Printf("%+v", stocks)
	stockRegexp := regexp.MustCompile(`(\d+)-(\d+)`)
	for _, stock := range stocks {
		if stock.OrderID.Valid {
			//log.Printf("%+v", stock)
		}
		m := stockRegexp.FindStringSubmatch(stock.SeatID)
		row, _ := strconv.Atoi(m[1])
		col, _ := strconv.Atoi(m[2])
		v.Stocks[row][col] = stock
	}
	return nil
}

func ticketGenerator() {
	tickets := []TicketWithArtist{}
	err := db.Select(&tickets, "SELECT t.*, a.name AS artist_name FROM ticket t INNER JOIN artist a ON t.artist_id = a.id ")
	if err != nil {
		panic(err)
	}
	for _, ticket := range tickets {
		timer := time.After(100 * time.Millisecond)
		<-timer
		go ticketItemGenerator(ticket.ID)
	}
}

func ticketItemGenerator(ticketid uint) {
	var ticket TicketWithArtist
	err := db.Get(&ticket, "SELECT t.*, a.name AS artist_name FROM ticket t INNER JOIN artist a ON t.artist_id = a.id WHERE t.id = ? LIMIT 1", ticketid)
	if err != nil {
		panic(err)
	}
	ticker := time.NewTicker(time.Millisecond * 200)
	for _ = range ticker.C {
		fmt.Printf("generate template: ticketid: %d\n", ticket.ID)
		renderTicketTemplate(fmt.Sprintf("%d", ticket.ID))
	}
	ticker.Stop()
}

func renderTicketTemplate(ticketid string) (string, error) {
	ticket := TicketWithArtist{}

	cacheKey := fmt.Sprintf("ticket_page%s", ticketid)
	err := db.Get(&ticket, `
		SELECT t.*, a.name AS artist_name FROM ticket t
		INNER JOIN artist a ON t.artist_id = a.id
		WHERE t.id = ? LIMIT 1`, ticketid)
	if err != nil {
		return "", err
	}
	//log.Printf("%+v", ticket)

	variations := []VariationWithStocks{}
	err = db.Select(&variations,
		`SELECT id, name FROM variation WHERE ticket_id = ? ORDER BY id`, ticketid)
	if err != nil {
		return "", err
	}
	//log.Printf("%+v", variations)

	c, err := connectRedis()
	if err != nil {
		return "", err
	}
	for i := 0; i < len(variations); i++ {
		v := &variations[i]

		num, err := redis.Int(c.Do("SCARD", fmt.Sprintf("stock_%d", v.ID)))
		v.Count = uint(num)
		if err != nil {
			return "", err
		}
	}

	recents, err := getRecentSold()
	if err != nil {
		return "", err
	}

	seatMaps := make([]template.HTML, len(variations))
	for i, variation := range variations {

		if err = loadStocksToVariation(&variation); err != nil {
			return "", err
		}
		seatCache := generateSeatMapCache(variation)
		seatMaps[i] = seatCache.Content
	}

	var buffer bytes.Buffer
	tmpl.ExecuteTemplate(&buffer, "ticket.html", map[string]interface{}{
		"recents":    recents,
		"ticket":     ticket,
		"seatMaps":   seatMaps,
		"variations": variations,
	})
	result := buffer.String()
	gocache.Set(cacheKey, result, 1*time.Second)
	return result, nil

}
