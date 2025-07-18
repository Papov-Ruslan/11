package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/eiannone/keyboard"
	"github.com/fatih/color"
	"github.com/gosuri/uilive"
	"github.com/guptarohit/asciigraph"
)

const (
	apiEndpoint       = "https://api.binance.com/api/v3"
	updateInterval    = 1 * time.Second
	maxDataPoints     = 100
	initialDataPoints = 10
)

type PriceData struct {
	Symbol string
	Prices []float64
	Times  []time.Time
	mu     sync.Mutex
}

type ApiResponse struct {
	Price string `json:"price"`
}

type TradeData struct {
	Price string `json:"price"`
}

var (
	green = color.New(color.FgGreen).SprintFunc()
)

func main() {
	// Обработка прерываний
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Инициализация клавиатуры
	if err := keyboard.Open(); err != nil {
		log.Fatal(err)
	}
	defer keyboard.Close()

	writer := uilive.New()
	writer.RefreshInterval = updateInterval
	writer.Start()
	defer writer.Stop()

	symbols := map[rune]string{
		'1': "BTCUSDT",
		'2': "ETHUSDT",
		'3': "LTCUSDT",
	}

	var (
		currentSymbol string
		priceData     *PriceData
		dataChan      = make(chan *PriceData, 10)
		stopWorker    = make(chan struct{})
		quitChan      = make(chan struct{})
		showMenuFlag  = true
	)

	// Функция показа меню
	showMenu := func() {
		writer.Flush()
		fmt.Fprintf(writer, "%s\n", green("Выберите криптовалюту:"))
		fmt.Fprintf(writer, "1. Bitcoin (BTC/USDT)\n")
		fmt.Fprintf(writer, "2. Ethereum (ETH/USDT)\n")
		fmt.Fprintf(writer, "3. Litecoin (LTC/USDT)\n")
		fmt.Fprintf(writer, "\nНажмите 1-3 для выбора, BACKSPACE для меню, q для выхода\n")
		writer.Flush()
		showMenuFlag = true
	}

	// Функция запуска воркера
	startWorker := func(symbol string) {
		close(stopWorker) // Остановить предыдущий воркер
		stopWorker = make(chan struct{})

		priceData = &PriceData{Symbol: symbol}
		if err := loadInitialData(symbol, priceData); err != nil {
			log.Printf("Ошибка загрузки данных: %v\n", err)
			return
		}
		dataChan <- priceData
		go priceWorker(symbol, dataChan, stopWorker)
		showMenuFlag = false
	}

	// Показать меню при старте
	showMenu()

	// Обработка ввода
	go func() {
		for {
			char, key, err := keyboard.GetKey()
			if err != nil {
				log.Println("Ошибка чтения клавиатуры:", err)
				continue
			}

			if key == keyboard.KeyEsc || char == 'q' || char == 'Q' {
				close(quitChan)
				return
			}

			if key == keyboard.KeyBackspace || key == keyboard.KeyBackspace2 {
				showMenu()
				continue
			}

			if newSymbol, ok := symbols[char]; ok {
				if newSymbol != currentSymbol {
					currentSymbol = newSymbol
					startWorker(currentSymbol)
				}
			}
		}
	}()

	// Основной цикл
mainLoop:
	for {
		select {
		case <-quitChan:
			break mainLoop
		case <-sigChan:
			break mainLoop
		case newData := <-dataChan:
			priceData = newData
			if !showMenuFlag {
				renderGraph(priceData, writer)
			}
		default:
			time.Sleep(50 * time.Millisecond)
		}
	}

	// Завершение работы
	close(stopWorker)
	writer.Stop()
	keyboard.Close()
	os.Exit(0)
}

func loadInitialData(symbol string, data *PriceData) error {
    resp, err := http.Get(fmt.Sprintf("%s/klines?symbol=%s&interval=1m&limit=%d", apiEndpoint, symbol, initialDataPoints))
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return err
    }

    var klines [][]interface{}
    if err := json.Unmarshal(body, &klines); err != nil {
        return err
    }

    data.mu.Lock()
    defer data.mu.Unlock()

    // Заполняем данные ценами закрытия
    for _, k := range klines {
        if len(k) < 5 {
            continue
        }
        priceStr, ok := k[4].(string)
        if !ok {
            continue
        }
        price, err := strconv.ParseFloat(priceStr, 64)
        if err != nil {
            return err
        }
        data.Prices = append(data.Prices, price)
        
        // Используем временную метку свечи
        timestamp, ok := k[0].(float64)
        if !ok {
            timestamp = float64(time.Now().Unix() * 1000)
        }
        data.Times = append(data.Times, time.Unix(int64(timestamp)/1000, 0))
    }

    // Если данных недостаточно, добавляем вариативности
    if len(data.Prices) < initialDataPoints {
        lastPrice := data.Prices[len(data.Prices)-1]
        // Добавляем небольшие колебания вокруг последней цены
        for i := len(data.Prices); i < initialDataPoints; i++ {
            variation := lastPrice * 0.0005 * float64(i%3-1) // ±0.05%
            data.Prices = append(data.Prices, lastPrice+variation)
            data.Times = append(data.Times, time.Now().Add(-time.Duration(initialDataPoints-i)*time.Minute))
        }
    }

    return nil
}

func priceWorker(symbol string, dataChan chan<- *PriceData, stop <-chan struct{}) {
    ticker := time.NewTicker(updateInterval)
    defer ticker.Stop()

    data := &PriceData{Symbol: symbol}

    for {
        select {
        case <-stop:
            return
        case <-ticker.C:
            price, err := fetchPrice(symbol)
            if err != nil {
                log.Println("Ошибка получения цены:", err)
                continue
            }

            data.mu.Lock()
            if len(data.Prices) < maxDataPoints {
                // Если данных меньше максимального количества, заполняем массив
                for i := len(data.Prices); i < maxDataPoints; i++ {
                    data.Prices = append(data.Prices, price)
                    data.Times = append(data.Times, time.Now())
                }
            } else {
                // Если данных достаточно, удаляем старую цену и добавляем новую
                data.Prices = append(data.Prices[1:], price)
                data.Times = append(data.Times[1:], time.Now())
            }
            data.mu.Unlock()

            dataChan <- data
        }
    }
}





func fetchPrice(symbol string) (float64, error) {
	resp, err := http.Get(fmt.Sprintf("%s/ticker/price?symbol=%s", apiEndpoint, symbol))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var apiResp ApiResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return 0, err
	}

	price, err := strconv.ParseFloat(apiResp.Price, 64)
	if err != nil {
		return 0, err
	}

	return price, nil
}

func renderGraph(data *PriceData, writer *uilive.Writer) {
	data.mu.Lock()
	defer data.mu.Unlock()

	if len(data.Prices) == 0 {
		return
	}

	graph := asciigraph.Plot(
		data.Prices,
		asciigraph.Width(100),
		asciigraph.Height(10),
		asciigraph.Precision(2),
	)

	writer.Flush()

	currentTime := time.Now().Format("15:04:05")
	fmt.Fprintf(writer.Newline(), "%s\n", green(data.Symbol))
	fmt.Fprintf(writer.Newline(), "%s\n", graph)
	fmt.Fprintf(writer.Newline(), "Последняя цена: %s | Время: %s\n",
		green(fmt.Sprintf("%.2f", data.Prices[len(data.Prices)-1])),
		currentTime)
	fmt.Fprintf(writer.Newline(), "Нажмите BACKSPACE для меню, q для выхода\n")
	writer.Flush()
}