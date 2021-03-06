package websocket

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/boltdb/bolt"
	"github.com/gorilla/websocket"

	book_info "github.com/lian/gdax-bookmap/exchanges/bitstamp/product_info"
	"github.com/lian/gdax-bookmap/exchanges/common/orderbook"
	"github.com/lian/gdax-bookmap/orderbook/product_info"
	"github.com/lian/gdax-bookmap/util"
)

type Client struct {
	Products    []string
	Books       map[string]*orderbook.Book
	Socket      *websocket.Conn
	DB          *bolt.DB
	dbEnabled   bool
	LastSync    time.Time
	LastDiff    time.Time
	LastDiffSeq uint64
	BatchWrite  map[string]*util.BookBatchWrite
	Infos       []*product_info.Info
}

func New(db *bolt.DB, products []string) *Client {
	c := &Client{
		Products:   []string{},
		Books:      map[string]*orderbook.Book{},
		BatchWrite: map[string]*util.BookBatchWrite{},
		DB:         db,
		Infos:      []*product_info.Info{},
	}

	if c.DB != nil {
		c.dbEnabled = true
	}

	for _, name := range products {
		c.AddProduct(name)
	}

	if c.dbEnabled {
		buckets := []string{}
		for _, info := range c.Infos {
			buckets = append(buckets, info.DatabaseKey)
		}
		util.CreateBucketsDB(db, buckets)
	}

	return c
}

func (c *Client) AddProduct(name string) {
	c.Products = append(c.Products, name)
	c.BatchWrite[name] = &util.BookBatchWrite{Count: 0, Batch: []*util.BatchChunk{}}
	book := orderbook.New(name)
	info := book_info.FetchProductInfo(name)
	c.Infos = append(c.Infos, &info)
	book.SetProductInfo(info)
	diff_channel, trades_channel := c.GetChannelNames(book)
	c.Books[diff_channel] = book
	c.Books[trades_channel] = book
}

func (c *Client) Connect() error {
	url := "wss://ws.pusherapp.com/app/de504dc5763aeef9ff52?protocol=7&client=js&version=2.1.6&flash=false"
	fmt.Println("connect to websocket", url)
	s, _, err := websocket.DefaultDialer.Dial(url, nil)

	if err != nil {
		return err
	}

	c.Socket = s

	for channel, _ := range c.Books {
		c.Subscribe(channel)
	}

	return nil
}

func (c *Client) Subscribe(channel string) {
	a := map[string]interface{}{"event": "pusher:subscribe", "data": map[string]interface{}{"channel": channel}}
	c.Socket.WriteJSON(a)
}

func (c *Client) GetChannelNames(book *orderbook.Book) (string, string) {
	if book.ID == "BTC-USD" {
		return "diff_order_book", "live_trades"
	} else {
		id := strings.ToLower(strings.Replace(book.ProductInfo.ID, "-", "", -1))
		return fmt.Sprintf("diff_order_book_%s", id), fmt.Sprintf("live_trades_%s", id)
	}
}

type Packet struct {
	Event   string `json:"event"`
	Channel string `json:"channel"`
	Data    string `json:"data"`
}

func (c *Client) UpdateSync(book *orderbook.Book, last uint64) error {
	seq := book.Sequence

	if last < seq {
		return fmt.Errorf("Ignore old messages %d %d", last, seq)
	}

	book.Sequence = last
	return nil
}

func (c *Client) HandleMessage(book *orderbook.Book, pkt Packet) {
	eventTime := time.Now()
	var trade *orderbook.Trade

	switch pkt.Event {
	case "data":
		//fmt.Println("diff", book.ID, string(pkt.Data))

		var data map[string]interface{}
		if err := json.Unmarshal([]byte(pkt.Data), &data); err != nil {
			log.Println(err)
			return
		}
		seq, _ := strconv.ParseInt(data["timestamp"].(string), 10, 64)

		if err := c.UpdateSync(book, uint64(seq)); err != nil {
			fmt.Println(err)
			return
		}

		for _, d := range data["bids"].([]interface{}) {
			data := d.([]interface{})
			price, _ := strconv.ParseFloat(data[0].(string), 64)
			size, _ := strconv.ParseFloat(data[1].(string), 64)
			book.UpdateBidLevel(eventTime, price, size)
		}

		for _, d := range data["asks"].([]interface{}) {
			data := d.([]interface{})
			price, _ := strconv.ParseFloat(data[0].(string), 64)
			size, _ := strconv.ParseFloat(data[1].(string), 64)
			book.UpdateAskLevel(eventTime, price, size)
		}

	case "trade":
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(pkt.Data), &data); err != nil {
			log.Println(err)
			return
		}

		price, _ := strconv.ParseFloat(data["price_str"].(string), 64)
		size, _ := strconv.ParseFloat(data["amount_str"].(string), 64)
		side := book.GetSide(price)

		book.AddTrade(eventTime, side, price, size)
		trade = book.Trades[len(book.Trades)-1]

	default:
		fmt.Println("unkown event", book.ID, pkt.Event, string(pkt.Data))
		return
	}

	if c.dbEnabled {
		batch := c.BatchWrite[book.ID]
		now := time.Now()
		if trade != nil {
			batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, orderbook.PackTrade(trade))
		}

		if batch.NextSync(now) {
			fmt.Println("STORE SYNC", book.ID, batch.Count)
			c.WriteSync(batch, book, now)
		} else {
			if batch.NextDiff(now) {
				//fmt.Println("STORE DIFF", book.ID, batch.Count)
				c.WriteDiff(batch, book, now)
			}
		}
	}
}

func (c *Client) WriteDiff(batch *util.BookBatchWrite, book *orderbook.Book, now time.Time) {
	book.FixBookLevels() // TODO fix/remove
	diff := book.Diff
	if len(diff.Bid) != 0 || len(diff.Ask) != 0 {
		pkt := orderbook.PackDiff(batch.LastDiffSeq, book.Sequence, diff)
		batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, pkt)
		book.ResetDiff()
		batch.LastDiffSeq = book.Sequence + 1
	}
}

func (c *Client) WriteSync(batch *util.BookBatchWrite, book *orderbook.Book, now time.Time) {
	book.FixBookLevels() // TODO fix/remove
	batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, orderbook.PackSync(book))
	book.ResetDiff()
	batch.LastDiffSeq = book.Sequence + 1
}

func (c *Client) Run() {
	for {
		c.run()
	}
}

func (c *Client) run() {
	if err := c.Connect(); err != nil {
		fmt.Println("failed to connect", err)
		time.Sleep(1000 * time.Millisecond)
		return
	}
	defer c.Socket.Close()

	for {
		msgType, message, err := c.Socket.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			return
		}

		if msgType != websocket.TextMessage {
			continue
		}

		var pkt Packet
		if err := json.Unmarshal(message, &pkt); err != nil {
			log.Println("header-parse:", err)
			continue
		}

		switch pkt.Event {
		// pusher stuff
		case "pusher:connection_established":
			log.Println("Connected")
			continue
		case "pusher_internal:subscription_succeeded":
			log.Println("Subscribed")
			continue
		case "pusher:pong":
			// ignore
			continue
		case "pusher:ping":
			c.Socket.WriteJSON(map[string]interface{}{"event": "pusher:pong"})
			continue
		}

		var ok bool
		var book *orderbook.Book

		if book, ok = c.Books[pkt.Channel]; !ok {
			log.Println("book not found", pkt.Channel)
			continue
		}

		if book.Sequence == 0 {
			c.SyncBook(book)
			continue
		}

		c.HandleMessage(book, pkt)
	}
}
