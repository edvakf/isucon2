# ISUCON2 Go版

```
go run go/app.go --port=5000 --appdir=~/go/src/github.com/edvakf/isucon2go/webapp/go
```

依存するもの

- `github.com/go-sql-driver/mysql`
- `github.com/gorilla/mux`
- `github.com/jmoiron/sqlx`

使いまわせそうな処理をメモ

## コマンドライン引数

```go
var port = flag.Uint("port", 5000, "port to listen")
var appDir = flag.String("appdir", ".", "the directory where public & views directories are located")

func main() {
	flag.Parse()
```

## Gorilla / mux でルーティング

```go
	r := mux.NewRouter()
	r.HandleFunc("/", indexHandler).Methods("GET")
	r.HandleFunc("/artist/{artistid:[0-9]+}", artistHandler).Methods("GET")
	r.HandleFunc("/ticket/{ticketid:[0-9]+}", ticketHandler).Methods("GET")
	r.HandleFunc("/ticket/{ticketid:[0-9]+}", ticketHandler).Methods("GET")
	r.HandleFunc("/buy", buyHandler).Methods("POST")
	r.HandleFunc("/admin", adminHandler).Methods("GET")
	r.HandleFunc("/admin/order.csv", adminOrderHandler).Methods("GET")
	r.HandleFunc("/admin", adminPostHandler).Methods("POST")
	r.PathPrefix("/").Handler(http.FileServer(http.Dir(getAppDir() + "/public/")))
	http.Handle("/", r)
	err := http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)
```

`http.FileServer` はディレクトリインデックスを表示してしまうので、オフにするには一手間かけないといけない

* https://groups.google.com/forum/#!topic/golang-nuts/bStLPdIVM6w

`r.Get` とか `r.Post` とか書きたければ、`github.com/drone/routes` とかを使えばいいらしい。

* http://www.alexedwards.net/blog/a-mux-showdown

ルーティングでワイルドカード定義したパラメータは `mux.Vars` で取得できる。

```go
	artistid := mux.Vars(r)["artistid"]
```

## POST パラメータ

```go
func buyHandler(w http.ResponseWriter, r *http.Request) {
	memberid := r.PostFormValue("member_id")
	variationid := r.PostFormValue("variation_id")
```

ちなみにGETパラメータはこうするらしい。

```go
r.URL.Query().Get("name")
```

## レスポンスヘッダーをセット

```go
	w.Header().Set("Content-Type", "text/csv")
```

## テンプレート

これについてはちょっと失敗した。一番最初にGlobですべてのテンプレートを読み込んで

```go
var tmpl *template.Template

func initTmpl() {
	tmpl = template.Must(template.ParseGlob(getAppDir() + "/views/*.html"))
}
```

使うときはこうすると良いだろうと思ったが、

```go
	tmpl.ExecuteTemplate(w, "complete.html", map[string]interface{}{
		"seatid":   seatid,
		"memberid": memberid,
	})
```

これだとテンプレート継承が使えない。なぜなら、Globでマッチするパターンの中に同じ名前の `define` が複数回出ることになってしまうため。

* http://qiita.com/peketamin/items/1b9b5c74fdb38d52ad77

とはいえ起動時に全てのテンプレートをGlobでパースしておくのはパフォーマンス上はだいぶ有利な気もする。

このようにlayout.htmlでlauoutTopとlayoutBottomに分割して、

```
{{define "layoutTop"}}
<!DOCTYPE html>
<html>
  <head>
  ...
{{end}}
{{define "layoutBottom"}}
    </div>
  </body>
</html>
{{end}}
```

すべてのテンプレートで上下部分をはめ込んでいる。カッコ悪い。

```
{{template "layoutTop" .}}
<span class="result" data-result="failure">売り切れました。</span>
{{template "layoutBottom" .}}
```

## MySQL接続

```go
import (
	"database/sql"
	_ "github.com/go-sql-driver/mysql"
)

var db *sql.DB
```

起動時に1回だけこれをやるだけで、標準のsqlがコネクションプーリングとか再接続処理もしてくれる。素晴らしい。

```go
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s",
		dbConf.Username, dbConf.Password, dbConf.Host, dbConf.Port, dbConf.DBname)
	db, err = sql.Open("mysql", dsn)
	err = db.Ping()
	if err != nil {
		panic(err.Error())
	}
```

### sqlxを使う

上のsqlをsqlxに変えるだけ。

```go
import (
	"database/sql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

var db *sqlx.DB

...

	db, err = sqlx.Open("mysql", dsn)
```

### sqlx: Select, Get

まず、標準のsqlパッケージは生で使うのはキツすぎる。例えば `rows.Scan` でSELECT結果を構造体に一括で割り当てられないなど。sqlxには `rows.StructScan` があったり、`Select` `Get` という便利なメソッドがある。

`Select` は複数。第一引数のスライスのポインターに対して結果を割り当ててくれる。

```go
	artists := []Artist{}
	err := db.Select(&artists, "SELECT * FROM artist ORDER BY id")
```

`Get` は単数。

```go
	artist := Artist{}
	err := db.Get(&artist, "SELECT id, name FROM artist WHERE id = ? LIMIT 1", artistid)
```

↑プレースホルダーも直感的に使える。（名前付きプレースホルダーは `Named` のほうのメソッドを呼ぶらしい）

### `StructScan`

上野2つさえあれば `StructScan` を使う場面はほとんど無い。標準の `Query` の代わりに `Queryx` を使えば `StructScan` が使える。

```go
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
```

### UPDATE系は `Exec` を使う。

```go
		l := strings.TrimSpace(line)
		if len(l) == 0 {
			continue
		}
		_, err = db.Exec(l)
```

### トランザクション

`tx.Get` をしているため、`db.Begin` ではなく `db.Beginx` を使ってsqlxのほうのトランザクションを生成している。

`Exec` の返り値の `Result` には `LastInsertId` と `RowsAffected` メソッドがある。

エラー処理が行数を食い過ぎるのどうにかしたい。

```go
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
```

### カラムがNullableなとき

受け取り手の構造体でNullなんちゃらを使う

```go
type Stock struct {
	ID          uint
	VariationID uint          `db:"variation_id"`
	SeatID      string        `db:"seat_id"`
	OrderID     sql.NullInt64 `db:"order_id"`
	UpdatedAt   string        `db:"updated_at"`
}
```

Null判定には `Valid` を使う。

```go
if stock.OrderID.Valid {
```

## 正規表現

1箇所だけ使った。

`MustCompile` はコンパイルできないときにパニックになってくれるので、エラーを受け取らなくても良くて便利。

```go
		for _, stock := range stocks {
			m := regexp.MustCompile(`(\d+)-(\d+)`).FindStringSubmatch(stock.SeatID)
			row, _ := strconv.Atoi(m[1])
			col, _ := strconv.Atoi(m[2])
			v.Stocks[row][col] = stock
		}
```

## `struct` の継承？

embedとかいうらしい。

よくわかってないがこういう感じでできる。

```go
type Ticket struct {
	ID       uint
	Name     string
	ArtistID uint `db:"artist_id"`
}

type TicketWithCount struct {
	Ticket
	Count int
}
```

# 以下、元のREADME

# ISUCON 2 #

- by sugyan
  - /webapp/perl
  - /webapp/ruby
  - /webapp/nodejs
- by faultier
  - /webapp/php
- by just\_do\_neet
  - /webapp/java
- by xxxxxx
  - /webapp/python
- by tagomoris
  - /tools

And many many patches by kazeburo.

## web app ##

- /webapp/perl
- /webapp/php
- /webapp/nodejs
- /webapp/ruby
- /webapp/python
- /webapp/java

### Webアプリの基本方針 ###

- 処理はすべてリクエストを受け取ってから実施する
  - DBへのクエリ
  - テンプレートからのレンダリング
- 全てのコンテンツをアプリケーションから渡す
  - js/css/画像も含めて
  - キャッシュ等はとりあえず全て無し

### 実装するリクエストハンドラ ###

- `/`
  - GET
  - artistのリスト
    - `SELECT * FROM artist ORDER BY id`

- `/artist/:artistid`
  - GET
  - ticketのリスト 合計の残り枚数表示
    - `SELECT id, name FROM artist WHERE id = ? LIMIT 1`
    - `SELECT id, name FROM ticket WHERE artist_id = ? ORDER BY id`
    - `SELECT COUNT(*) FROM variation INNER JOIN stock ON stock.variation_id = variation.id WHERE variation.ticket_id = ? AND stock.order_id IS NULL`

- `/ticket/:ticket`
  - GET
  - variationのリスト 各種残り枚数表示
    - `SELECT t.*, a.name AS artist_name FROM ticket t INNER JOIN artist a ON t.artist_id = a.id WHERE t.id = ? LIMIT 1`
    - `SELECT id, name FROM variation WHERE ticket_id = ? ORDER BY id`
    - `SELECT seat_id, order_id FROM stock WHERE variation_id = ?`
    - `SELECT COUNT(*) FROM stock WHERE variation_id = ? AND order_id IS NULL`
  
- `/buy`
  - POST
  - チケットの購入 stockの在庫を1つ抑え席番を表示 `member_id`を受け取り`order_request`に保存
    - `START TRANSACTION`
    - `INSERT INTO order_request (member_id) VALUES (?)`
    - `UPDATE stock SET order_id = ? WHERE variation_id = ? AND order_id IS NULL ORDER BY RAND() LIMIT 1`
    - `COMMIT`

- なお、全ページ左側のサイドバーに「最近購入されたチケット10件」を表示
 - ```
SELECT stock.seat_id, variation.name AS v_name, ticket.name AS t_name, artist.name AS a_name FROM stock
  JOIN variation ON stock.variation_id = variation.id
  JOIN ticket ON variation.ticket_id = ticket.id
  JOIN artist ON ticket.artist_id = artist.id
WHERE order_id IS NOT NULL
ORDER BY order_id DESC LIMIT 10
```

### staticファイル ###

- images
 - isucon_title ロゴ
- js
 - jquery 最新版minified
 - jquery-ui 最新版minified
 - isucon2.js
- css
 - jquery-ui ui-lightness
 - isucon2.css デザイン調整用

## benchmark tool ##

- /tools
