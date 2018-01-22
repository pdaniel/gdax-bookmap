package websocket

// https://github.com/binance-exchange/binance-official-api-docs/blob/master/web-socket-streams.md
// https://github.com/binance-exchange/binance-official-api-docs/blob/master/rest-api.md

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/boltdb/bolt"
	"github.com/gorilla/websocket"
	"github.com/lian/gdax-bookmap/binance/orderbook"
	"github.com/lian/gdax-bookmap/orderbook/product_info"
	"github.com/lian/gdax-bookmap/util"
)

type Client struct {
	Socket      *websocket.Conn
	Products    []string
	Books       map[string]*orderbook.Book
	ConnectedAt time.Time
	DB          *bolt.DB
	dbEnabled   bool
	BatchWrite  map[string]*util.BookBatchWrite
	Infos       []*product_info.Info
}

func New(db *bolt.DB, bookUpdated, tradesUpdated chan string) *Client {
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

	// https://api.binance.com/api/v1/exchangeInfo

	products := []string{"BTC-USDT"}
	for _, name := range products {
		c.AddProduct(name)
	}

	if c.dbEnabled {
		buckets := []string{}
		for _, info := range c.Infos {
			buckets = append(buckets, info.DatabaseKey)
		}
		util.CreateBucketsDB(c.DB, buckets)
	}

	return c
}

func streamNames(name string) (string, string) {
	return name + "@depth", name + "@aggTrade"
}

func (c *Client) AddProduct(name string) {
	c.Products = append(c.Products, name)
	c.BatchWrite[name] = &util.BookBatchWrite{Count: 0, Batch: []*util.BatchChunk{}}
	book := orderbook.New(name)
	info := orderbook.FetchProductInfo(name)
	c.Infos = append(c.Infos, &info)
	a, b := streamNames(strings.ToLower(info.ID))
	c.Books[a] = book
	c.Books[b] = book
}

func (c *Client) Connect() {
	streams := []string{}
	for _, name := range c.Products {
		info := orderbook.FetchProductInfo(name)
		a, b := streamNames(strings.ToLower(info.ID))
		streams = append(streams, a)
		streams = append(streams, b)
	}
	url := "wss://stream.binance.com:9443/stream?streams=" + strings.Join(streams, "/")

	fmt.Println("connect to websocket", url)
	s, _, err := websocket.DefaultDialer.Dial(url, nil)
	c.Socket = s

	if err != nil {
		log.Fatal("dial:", err)
	}

	c.ConnectedAt = time.Now()
}

type PacketHeader struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type PacketEventHeader struct {
	EventType string `json:"e"`
	EventTime int    `json:"E"`
	//Symbol        string        `json:"s"`
}

type PacketDepthUpdate struct {
	//EventType     string        `json:"e"`
	//EventTime     int           `json:"E"`
	//Symbol        string        `json:"s"`
	FirstUpdateID uint64        `json:"U"`
	FinalUpdateID uint64        `json:"u"`
	Bids          []interface{} `json:"b"` // [ "price", "quantity", []]
	Asks          []interface{} `json:"a"` // [ "price", "quantity", []]
}

type PacketAggTrade struct {
	//EventType        string `json:"e"`
	//EventTime        int    `json:"E"`
	//Symbol           string `json:"s"`
	//AggregateTradeID int    `json:"a"`
	//TradeTime        int    `json:"T"`
	Price         string `json:"p"`
	Quantity      string `json:"q"`
	FirstUpdateID int    `json:"f"`
	FinalUpdateID int    `json:"l"`
	BuyMaker      bool   `json:"m"`
	Ignore        bool   `json:"M"`
}

func (c *Client) UpdateSync(book *orderbook.Book, first, last uint64) error {
	seq := book.Sequence
	next := seq + 1

	if first <= seq {
		return fmt.Errorf("Ignore old messages %d %d", last, seq)
	}

	if book.Synced {
		if first != next {
			c.SyncBook(book)
			return fmt.Errorf("Message lost, resync")
		}
	} else {
		if (first <= next) && (last >= next) {
			book.Synced = true
		}
	}

	book.Sequence = last
	return nil
}

func (c *Client) HandleMessage(book *orderbook.Book, raw json.RawMessage) {
	var event PacketEventHeader
	if err := json.Unmarshal(raw, &event); err != nil {
		log.Println("PacketEventType-parse:", err)
		return
	}

	eventTime := time.Unix(0, int64(event.EventTime)*int64(time.Millisecond))
	var trade *orderbook.Trade

	switch event.EventType {
	case "depthUpdate":
		var depthUpdate PacketDepthUpdate
		if err := json.Unmarshal(raw, &depthUpdate); err != nil {
			log.Println("PacketDepthUpdate-parse:", err)
			return
		}

		if err := c.UpdateSync(book, uint64(depthUpdate.FirstUpdateID), uint64(depthUpdate.FinalUpdateID)); err != nil {
			fmt.Println(err)
			return
		}

		for _, d := range depthUpdate.Bids {
			data := d.([]interface{})
			price, _ := strconv.ParseFloat(data[0].(string), 64)
			size, _ := strconv.ParseFloat(data[1].(string), 64)
			book.UpdateBidLevel(eventTime, price, size)
		}

		for _, d := range depthUpdate.Asks {
			data := d.([]interface{})
			price, _ := strconv.ParseFloat(data[0].(string), 64)
			size, _ := strconv.ParseFloat(data[1].(string), 64)
			book.UpdateAskLevel(eventTime, price, size)
		}

	case "aggTrade":
		var data PacketAggTrade
		if err := json.Unmarshal(raw, &data); err != nil {
			log.Println("PacketDepthUpdate-parse:", err)
			return
		}

		price, _ := strconv.ParseFloat(data.Price, 64)
		size, _ := strconv.ParseFloat(data.Quantity, 64)

		side := book.GetSide(price)
		book.AddTrade(eventTime, side, price, size)
		trade = book.Trades[len(book.Trades)-1]

	default:
		fmt.Println("unkown event", book.ID, event.EventType, string(raw))
		return
	}

	if c.dbEnabled {
		batch := c.BatchWrite[book.ID]
		now := time.Now()
		if trade != nil {
			batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, PackTrade(trade))
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
		pkt := PackDiff(batch.LastDiffSeq, book.Sequence, diff)
		batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, pkt)
		book.ResetDiff()
		batch.LastDiffSeq = book.Sequence + 1
	}
}

func (c *Client) WriteSync(batch *util.BookBatchWrite, book *orderbook.Book, now time.Time) {
	book.FixBookLevels() // TODO fix/remove
	batch.Write(c.DB, now, book.ProductInfo.DatabaseKey, PackSync(book))
	book.ResetDiff()
	batch.LastDiffSeq = book.Sequence + 1
}

func (c *Client) Run() {
	for {
		c.run()
	}
}

func (c *Client) GetBook(id string) *orderbook.Book {
	info := orderbook.FetchProductInfo(id)
	key := strings.ToLower(info.ID) + "@depth"
	return c.Books[key]
}

func (c *Client) run() {
	c.Connect()
	defer c.Socket.Close()

	initialSync := true

	for {
		msgType, message, err := c.Socket.ReadMessage()
		if err != nil {
			log.Println("read:", err)
			return
		}

		if msgType != websocket.TextMessage {
			continue
		}

		if initialSync {
			for k, book := range c.Books {
				if strings.Contains(k, "@depth") {
					if err := c.SyncBook(book); err != nil {
						fmt.Println("initialSync-error", err)
					}
				}
			}
			initialSync = false
			continue
		}

		var pkt PacketHeader
		if err := json.Unmarshal(message, &pkt); err != nil {
			log.Println("PacketHeader-parse:", err)
			continue
		}

		var book *orderbook.Book
		var ok bool
		if book, ok = c.Books[pkt.Stream]; !ok {
			log.Println("book not found", pkt.Stream)
			continue
		}

		c.HandleMessage(book, pkt.Data)
	}
}
