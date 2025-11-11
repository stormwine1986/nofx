package market

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/markcheno/go-talib"
)

// FundingRateCache 资金费率缓存结构
// Binance Funding Rate 每 8 小时才更新一次，使用 1 小时缓存可显著减少 API 调用
type FundingRateCache struct {
	Rate      float64
	UpdatedAt time.Time
}

var (
	fundingRateMap sync.Map // map[string]*FundingRateCache
	frCacheTTL     = 1 * time.Hour
)

// Get 获取指定代币的市场数据
func Get(symbol string) (*Data, error) {
	var klines5m, klines1h, klines4h []Kline
	var err error
	// 标准化symbol
	symbol = Normalize(symbol)
	// 获取5分钟K线数据 (最近100个)
	klines5m, err = WSMonitorCli.GetCurrentKlines(symbol, "5m") // 多获取一些用于计算
	if err != nil {
		return nil, fmt.Errorf("获取5分钟K线失败: %v", err)
	}

	// 获取1小时K线数据 (最近100个)
	klines1h, err = WSMonitorCli.GetCurrentKlines(symbol, "1h") // 多获取一些用于计算
	if err != nil {
		return nil, fmt.Errorf("获取1小时K线失败: %v", err)
	}

	// 获取4小时K线数据 (最近100个)
	klines4h, err = WSMonitorCli.GetCurrentKlines(symbol, "4h") // 多获取用于计算指标
	if err != nil {
		return nil, fmt.Errorf("获取4小时K线失败: %v", err)
	}

	// 检查数据是否为空
	if len(klines5m) == 0 {
		return nil, fmt.Errorf("5分钟K线数据为空")
	}
	if len(klines1h) == 0 {
		return nil, fmt.Errorf("1小时K线数据为空")
	}
	if len(klines4h) == 0 {
		return nil, fmt.Errorf("4小时K线数据为空")
	}

	// 当前价格 (基于5分钟最新数据)
	currentPrice := klines5m[len(klines5m)-1].Close
	// currentEMA20 := calculateEMA(klines5m, 20)
	// currentMACD := calculateMACD(klines5m)
	// currentRSI7 := calculateRSI(klines5m, 7)

	// 计算价格变化百分比
	// 1小时价格变化 = 13个5分钟K线前的价格
	priceChange1h := 0.0
	if len(klines5m) >= 13 { // 至少需要13根K线 (当前 + 12根前)
		price1hAgo := klines5m[len(klines5m)-13].Close
		if price1hAgo > 0 {
			priceChange1h = ((currentPrice - price1hAgo) / price1hAgo) * 100
		}
	}

	// 4小时价格变化 = 1个4小时K线前的价格
	priceChange4h := 0.0
	if len(klines4h) >= 2 {
		price4hAgo := klines4h[len(klines4h)-2].Close
		if price4hAgo > 0 {
			priceChange4h = ((currentPrice - price4hAgo) / price4hAgo) * 100
		}
	}

	// 获取OI数据
	oiData, err := getOpenInterestData(symbol)
	if err != nil {
		// OI失败不影响整体,使用默认值
		oiData = &OIData{Latest: 0, Average: 0}
	}

	// 获取Funding Rate
	fundingRate, _ := getFundingRate(symbol)

	// 计算日内系列数据
	intradayData := calculateIntradaySeries(klines5m)

	// 计算中期数据
	middleTermData := calculateLongerTermData(klines1h)

	// 计算长期数据
	longerTermData := calculateLongerTermData(klines4h)

	return &Data{
		Symbol:        symbol,
		CurrentPrice:  currentPrice,
		PriceChange1h: priceChange1h,
		PriceChange4h: priceChange4h,
		// CurrentEMA20:      currentEMA20,
		// CurrentMACD:       currentMACD,
		// CurrentRSI7:       currentRSI7,
		OpenInterest:      oiData,
		FundingRate:       fundingRate,
		IntradaySeries:    intradayData,
		MiddleTermContext: middleTermData,
		LongerTermContext: longerTermData,
	}, nil
}

// calculateEMA 计算EMA 废弃
func calculateEMA(klines []Kline, period int) float64 {
	if len(klines) < period {
		return 0
	}

	// 计算SMA作为初始EMA
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += klines[i].Close
	}
	ema := sum / float64(period)

	// 计算EMA
	multiplier := 2.0 / float64(period+1)
	for i := period; i < len(klines); i++ {
		ema = (klines[i].Close-ema)*multiplier + ema
	}

	return ema
}

// calculateMACD 计算MACD 废弃
func calculateMACD(klines []Kline) float64 {
	if len(klines) < 26 {
		return 0
	}

	// 计算12期和26期EMA
	ema12 := calculateEMA(klines, 12)
	ema26 := calculateEMA(klines, 26)

	// MACD = EMA12 - EMA26
	return ema12 - ema26
}

// calculateRSI 计算RSI 废弃
func calculateRSI(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	gains := 0.0
	losses := 0.0

	// 计算初始平均涨跌幅
	for i := 1; i <= period; i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			gains += change
		} else {
			losses += -change
		}
	}

	avgGain := gains / float64(period)
	avgLoss := losses / float64(period)

	// 使用Wilder平滑方法计算后续RSI
	for i := period + 1; i < len(klines); i++ {
		change := klines[i].Close - klines[i-1].Close
		if change > 0 {
			avgGain = (avgGain*float64(period-1) + change) / float64(period)
			avgLoss = (avgLoss * float64(period-1)) / float64(period)
		} else {
			avgGain = (avgGain * float64(period-1)) / float64(period)
			avgLoss = (avgLoss*float64(period-1) + (-change)) / float64(period)
		}
	}

	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return rsi
}

// calculateATR 计算ATR 废弃
func calculateATR(klines []Kline, period int) float64 {
	if len(klines) <= period {
		return 0
	}

	trs := make([]float64, len(klines))
	for i := 1; i < len(klines); i++ {
		high := klines[i].High
		low := klines[i].Low
		prevClose := klines[i-1].Close

		tr1 := high - low
		tr2 := math.Abs(high - prevClose)
		tr3 := math.Abs(low - prevClose)

		trs[i] = math.Max(tr1, math.Max(tr2, tr3))
	}

	// 计算初始ATR
	sum := 0.0
	for i := 1; i <= period; i++ {
		sum += trs[i]
	}
	atr := sum / float64(period)

	// Wilder平滑
	for i := period + 1; i < len(klines); i++ {
		atr = (atr*float64(period-1) + trs[i]) / float64(period)
	}

	return atr
}

// calculateIntradaySeries 计算日内系列数据
func calculateIntradaySeries(klines []Kline) *IntradayData {
	// data := &IntradayData{
	// 	ClosePrices: make([]float64, 0, 10),
	// 	// EMA20Values: make([]float64, 0, 10),
	// 	// MACDValues:  make([]float64, 0, 10),
	// 	RSI7Values: make([]float64, 0, 10),
	// 	// RSI14Values: make([]float64, 0, 10),
	// }

	// 提取收盘价序列，用于后续指标计算
	closes := make([]float64, len(klines))
	for i, k := range klines {
		closes[i] = k.Close
	}

	// RSI_7
	rsi7 := talib.Rsi(closes, 7)

	// 获取最近10个数据点
	// start := len(klines) - 10
	// if start < 0 {
	// 	start = 0
	// }

	// for i := start; i < len(klines); i++ {
	// 	data.ClosePrices = append(data.ClosePrices, klines[i].Close)

	// 	// 计算每个点的EMA20
	// 	if i >= 19 {
	// 		ema20 := calculateEMA(klines[:i+1], 20)
	// 		data.EMA20Values = append(data.EMA20Values, ema20)
	// 	}

	// 计算每个点的MACD
	// if i >= 25 {
	// 	macd := calculateMACD(klines[:i+1])
	// 	data.MACDValues = append(data.MACDValues, macd)
	// }

	// 计算每个点的RSI
	// if i >= 7 {
	// 	rsi7 := calculateRSI(klines[:i+1], 7)
	// 	data.RSI7Values = append(data.RSI7Values, rsi7)
	// }
	// if i >= 14 {
	// 	rsi14 := calculateRSI(klines[:i+1], 14)
	// 	data.RSI14Values = append(data.RSI14Values, rsi14)
	// }
	// }

	return &IntradayData{
		ClosePrices: closes,
		RSI7Values:  rsi7,
	}
}

// calculateLongerTermData 计算长期数据
func calculateLongerTermData(klines []Kline) *LongerTermData {
	data := &LongerTermData{
		MACDValues:  make([]float64, 0, 10),
		RSI14Values: make([]float64, 0, 10),
	}

	// 提取序列，用于后续指标计算
	closes := make([]float64, len(klines))
	highs := make([]float64, len(klines))
	lows := make([]float64, len(klines))
	vols := make([]float64, len(klines))
	for i, k := range klines {
		closes[i] = k.Close
		highs[i] = k.High
		lows[i] = k.Low
		vols[i] = k.Volume
	}

	data.ClosePrices = closes

	// 计算EMA
	// data.EMA20 = calculateEMA(klines, 20)
	// data.EMA50 = calculateEMA(klines, 50)
	data.EMA20 = talib.Ema(closes, 20)
	data.EMA50 = talib.Ema(closes, 50)

	// 计算ATR
	// data.ATR3 = calculateATR(klines, 3)
	// data.ATR14 = calculateATR(klines, 14)
	data.ATR5 = talib.Atr(highs, lows, closes, 5)

	// 计算成交量
	// if len(klines) > 0 {
	// 	data.CurrentVolume = klines[len(klines)-1].Volume
	// 	// 计算平均成交量
	// 	sum := 0.0
	// 	for _, k := range klines {
	// 		sum += k.Volume
	// 	}
	// 	data.AverageVolume = sum / float64(len(klines))
	// }
	data.Volume = vols
	data.AverageVolume = talib.Sma(vols, 10)

	// 计算MACD和RSI序列
	// start := len(klines) - 10
	// if start < 0 {
	// 	start = 0
	// }

	// for i := start; i < len(klines); i++ {
	// 	// 价格序列
	// 	data.MidPrices = append(data.MidPrices, klines[i].Close)

	// 	if i >= 25 {
	// 		macd := calculateMACD(klines[:i+1])
	// 		data.MACDValues = append(data.MACDValues, macd)
	// 	}
	// 	if i >= 14 {
	// 		rsi14 := calculateRSI(klines[:i+1], 14)
	// 		data.RSI14Values = append(data.RSI14Values, rsi14)
	// 	}
	// }
	_, _, data.MACDValues = talib.Macd(closes, 12, 26, 9)
	data.RSI14Values = talib.Rsi(closes, 14)

	return data
}

// getOpenInterestData 获取OI数据
func getOpenInterestData(symbol string) (*OIData, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/openInterest?symbol=%s", symbol)

	apiClient := NewAPIClient()
	resp, err := apiClient.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OpenInterest string `json:"openInterest"`
		Symbol       string `json:"symbol"`
		Time         int64  `json:"time"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	oi, _ := strconv.ParseFloat(result.OpenInterest, 64)

	return &OIData{
		Latest:  oi,
		Average: oi * 0.999, // 近似平均值
	}, nil
}

// getFundingRate 获取资金费率（优化：使用 1 小时缓存）
func getFundingRate(symbol string) (float64, error) {
	// 检查缓存（有效期 1 小时）
	// Funding Rate 每 8 小时才更新，1 小时缓存非常合理
	if cached, ok := fundingRateMap.Load(symbol); ok {
		cache := cached.(*FundingRateCache)
		if time.Since(cache.UpdatedAt) < frCacheTTL {
			// 缓存命中，直接返回
			return cache.Rate, nil
		}
	}

	// 缓存过期或不存在，调用 API
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/premiumIndex?symbol=%s", symbol)

	apiClient := NewAPIClient()
	resp, err := apiClient.client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
		InterestRate    string `json:"interestRate"`
		Time            int64  `json:"time"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	rate, _ := strconv.ParseFloat(result.LastFundingRate, 64)

	// 更新缓存
	fundingRateMap.Store(symbol, &FundingRateCache{
		Rate:      rate,
		UpdatedAt: time.Now(),
	})

	return rate, nil
}

// Format 格式化输出市场数据
func Format(data *Data) string {
	var sb strings.Builder

	// 使用动态精度格式化价格
	priceStr := formatPriceWithDynamicPrecision(data.CurrentPrice)
	// sb.WriteString(fmt.Sprintf("current_price = %s, current_ema20 = %.3f, current_macd = %.3f, current_rsi (7 period) = %.3f\n\n",
	// 	priceStr, data.CurrentEMA20, data.CurrentMACD, data.CurrentRSI7))
	sb.WriteString(fmt.Sprintf("current_price = %s, Funding Rate: %.2e, Open Interest: %s\n\n",
		priceStr, data.FundingRate, formatPriceWithDynamicPrecision(data.OpenInterest.Latest)))

	// sb.WriteString(fmt.Sprintf("In addition, here is the latest %s open interest and funding rate for perps:\n\n",
	// 	data.Symbol))

	// if data.OpenInterest != nil {
	// 	// 使用动态精度格式化 OI 数据
	// 	oiLatestStr := formatPriceWithDynamicPrecision(data.OpenInterest.Latest)
	// 	oiAverageStr := formatPriceWithDynamicPrecision(data.OpenInterest.Average)
	// 	sb.WriteString(fmt.Sprintf("Open Interest: Latest: %s Average: %s\n\n",
	// 		oiLatestStr, oiAverageStr))
	// }

	// sb.WriteString(fmt.Sprintf("Funding Rate: %.2e\n\n", data.FundingRate))

	if data.IntradaySeries != nil {
		sb.WriteString("Intraday series (5‑minute intervals, oldest → latest):\n\n")

		// 取已收盘的最近20个值
		startIndex := len(data.IntradaySeries.ClosePrices) - 21
		endIndex := len(data.IntradaySeries.ClosePrices) - 1

		if len(data.IntradaySeries.ClosePrices) > 0 {
			sb.WriteString(fmt.Sprintf("Close prices: %s\n\n",
				formatFloatSlice(data.IntradaySeries.ClosePrices[startIndex:endIndex])))
		}

		// if len(data.IntradaySeries.EMA20Values) > 0 {
		// 	sb.WriteString(fmt.Sprintf("EMA indicators (20‑period): %s\n\n", formatFloatSlice(data.IntradaySeries.EMA20Values)))
		// }

		// if len(data.IntradaySeries.MACDValues) > 0 {
		// 	sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.IntradaySeries.MACDValues)))
		// }

		if len(data.IntradaySeries.RSI7Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI indicators (7‑Period): %s\n\n",
				formatFloatSlice(data.IntradaySeries.RSI7Values[startIndex:endIndex])))
		}

		// if len(data.IntradaySeries.RSI14Values) > 0 {
		// 	sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.IntradaySeries.RSI14Values)))
		// }
	}

	// 打印中期指标
	if data.MiddleTermContext != nil {
		sb.WriteString("Middle‑term context (1‑hour timeframe, oldest → latest):\n\n")

		// 取已收盘的最近20个值
		startIndex := len(data.IntradaySeries.ClosePrices) - 21
		endIndex := len(data.IntradaySeries.ClosePrices) - 1

		if len(data.MiddleTermContext.ClosePrices) > 0 {
			sb.WriteString(fmt.Sprintf("Close prices: %s\n\n",
				formatFloatSlice(data.MiddleTermContext.ClosePrices[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.EMA20) > 0 {
			sb.WriteString(fmt.Sprintf("EMA (20‑Period): %s\n\n",
				formatFloatSlice(data.MiddleTermContext.EMA20[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.EMA50) > 0 {
			sb.WriteString(fmt.Sprintf("EMA (50‑Period): %s\n\n",
				formatFloatSlice(data.MiddleTermContext.EMA50[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.ATR5) > 0 {
			sb.WriteString(fmt.Sprintf("ATR (5‑Period): %s\n\n",
				formatFloatSlice(data.MiddleTermContext.ATR5[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n",
				formatFloatSlice(data.MiddleTermContext.Volume[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.AverageVolume) > 0 {
			sb.WriteString(fmt.Sprintf("AverageVolume (10-Period): %s\n\n",
				formatFloatSlice(data.MiddleTermContext.AverageVolume[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.MACDValues) > 0 {
			sb.WriteString(fmt.Sprintf("MACD: %s\n\n",
				formatFloatSlice(data.MiddleTermContext.MACDValues[startIndex:endIndex])))
		}

		if len(data.MiddleTermContext.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI (14-Period): %s\n\n",
				formatFloatSlice(data.MiddleTermContext.RSI14Values[startIndex:endIndex])))
		}

		// sb.WriteString(fmt.Sprintf("20‑Period EMA: %.3f vs. 50‑Period EMA: %.3f\n\n",
		// 	data.MiddleTermContext.EMA20, data.MiddleTermContext.EMA50))

		// sb.WriteString(fmt.Sprintf("3‑Period ATR: %.3f vs. 14‑Period ATR: %.3f\n\n",
		// 	data.MiddleTermContext.ATR3, data.MiddleTermContext.ATR14))

		// sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
		// 	data.MiddleTermContext.CurrentVolume, data.MiddleTermContext.AverageVolume))

		// if len(data.MiddleTermContext.MACDValues) > 0 {
		// 	sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.MiddleTermContext.MACDValues)))
		// }

		// if len(data.MiddleTermContext.RSI14Values) > 0 {
		// 	sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.MiddleTermContext.RSI14Values)))
		// }
	}

	if data.LongerTermContext != nil {
		sb.WriteString("Longer‑term context (4‑hour timeframe, oldest → latest):\n\n")

		// 取已收盘的最近20个值
		startIndex := len(data.IntradaySeries.ClosePrices) - 21
		endIndex := len(data.IntradaySeries.ClosePrices) - 1

		if len(data.LongerTermContext.ClosePrices) > 0 {
			sb.WriteString(fmt.Sprintf("Close prices: %s\n\n",
				formatFloatSlice(data.LongerTermContext.ClosePrices[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.EMA20) > 0 {
			sb.WriteString(fmt.Sprintf("EMA (20‑Period): %s\n\n",
				formatFloatSlice(data.LongerTermContext.EMA20[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.EMA50) > 0 {
			sb.WriteString(fmt.Sprintf("EMA (50‑Period): %s\n\n",
				formatFloatSlice(data.LongerTermContext.EMA50[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.ATR5) > 0 {
			sb.WriteString(fmt.Sprintf("ATR (5‑Period): %s\n\n",
				formatFloatSlice(data.LongerTermContext.ATR5[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.Volume) > 0 {
			sb.WriteString(fmt.Sprintf("Volume: %s\n\n",
				formatFloatSlice(data.LongerTermContext.Volume[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.AverageVolume) > 0 {
			sb.WriteString(fmt.Sprintf("AverageVolume (10-Period): %s\n\n",
				formatFloatSlice(data.LongerTermContext.AverageVolume[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.MACDValues) > 0 {
			sb.WriteString(fmt.Sprintf("MACD: %s\n\n",
				formatFloatSlice(data.LongerTermContext.MACDValues[startIndex:endIndex])))
		}

		if len(data.LongerTermContext.RSI14Values) > 0 {
			sb.WriteString(fmt.Sprintf("RSI (14-Period): %s\n\n",
				formatFloatSlice(data.LongerTermContext.RSI14Values[startIndex:endIndex])))
		}

		// sb.WriteString(fmt.Sprintf("20‑Period EMA: %.3f vs. 50‑Period EMA: %.3f\n\n",
		// 	data.LongerTermContext.EMA20, data.LongerTermContext.EMA50))

		// // sb.WriteString(fmt.Sprintf("3‑Period ATR: %.3f vs. 14‑Period ATR: %.3f\n\n",
		// // 	data.LongerTermContext.ATR3, data.LongerTermContext.ATR14))

		// sb.WriteString(fmt.Sprintf("Current Volume: %.3f vs. Average Volume: %.3f\n\n",
		// 	data.LongerTermContext.Volume, data.LongerTermContext.AverageVolume))

		// if len(data.LongerTermContext.MACDValues) > 0 {
		// 	sb.WriteString(fmt.Sprintf("MACD indicators: %s\n\n", formatFloatSlice(data.LongerTermContext.MACDValues)))
		// }

		// if len(data.LongerTermContext.RSI14Values) > 0 {
		// 	sb.WriteString(fmt.Sprintf("RSI indicators (14‑Period): %s\n\n", formatFloatSlice(data.LongerTermContext.RSI14Values)))
		// }
	}

	return sb.String()
}

// formatPriceWithDynamicPrecision 根据价格区间动态选择精度
// 这样可以完美支持从超低价 meme coin (< 0.0001) 到 BTC/ETH 的所有币种
func formatPriceWithDynamicPrecision(price float64) string {
	switch {
	case price < 0.0001:
		// 超低价 meme coin: 1000SATS, 1000WHY, DOGS
		// 0.00002070 → "0.00002070" (8位小数)
		return fmt.Sprintf("%.8f", price)
	case price < 0.001:
		// 低价 meme coin: NEIRO, HMSTR, HOT, NOT
		// 0.00015060 → "0.000151" (6位小数)
		return fmt.Sprintf("%.6f", price)
	case price < 0.01:
		// 中低价币: PEPE, SHIB, MEME
		// 0.00556800 → "0.005568" (6位小数)
		return fmt.Sprintf("%.6f", price)
	case price < 1.0:
		// 低价币: ASTER, DOGE, ADA, TRX
		// 0.9954 → "0.9954" (4位小数)
		return fmt.Sprintf("%.4f", price)
	case price < 100:
		// 中价币: SOL, AVAX, LINK, MATIC
		// 23.4567 → "23.4567" (4位小数)
		return fmt.Sprintf("%.4f", price)
	default:
		// 高价币: BTC, ETH (节省 Token)
		// 45678.9123 → "45678.91" (2位小数)
		return fmt.Sprintf("%.2f", price)
	}
}

// formatFloatSlice 格式化float64切片为字符串（使用动态精度）
func formatFloatSlice(values []float64) string {
	strValues := make([]string, len(values))
	for i, v := range values {
		strValues[i] = formatPriceWithDynamicPrecision(v)
	}
	return "[" + strings.Join(strValues, ", ") + "]"
}

// Normalize 标准化symbol,确保是USDT交易对
func Normalize(symbol string) string {
	symbol = strings.ToUpper(symbol)
	if strings.HasSuffix(symbol, "USDT") {
		return symbol
	}
	return symbol + "USDT"
}

// parseFloat 解析float值
func parseFloat(v interface{}) (float64, error) {
	switch val := v.(type) {
	case string:
		return strconv.ParseFloat(val, 64)
	case float64:
		return val, nil
	case int:
		return float64(val), nil
	case int64:
		return float64(val), nil
	default:
		return 0, fmt.Errorf("unsupported type: %T", v)
	}
}
