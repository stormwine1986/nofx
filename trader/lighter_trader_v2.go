package trader

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	lighterClient "github.com/elliottech/lighter-go/client"
	lighterHTTP "github.com/elliottech/lighter-go/client/http"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// AccountInfo LIGHTER 賬戶信息
type AccountInfo struct {
	AccountIndex int64  `json:"account_index"`
	L1Address    string `json:"l1_address"`
	// 其他字段可以根據實際 API 響應添加
}

// LighterTraderV2 使用官方 lighter-go SDK 的新實現
type LighterTraderV2 struct {
	ctx        context.Context
	privateKey *ecdsa.PrivateKey // L1 錢包私鑰（用於識別賬戶）
	walletAddr string            // Ethereum 錢包地址

	client  *http.Client
	baseURL string
	testnet bool
	chainID uint32

	// SDK 客戶端
	httpClient lighterClient.MinimalHTTPClient
	txClient   *lighterClient.TxClient

	// API Key 管理
	apiKeyPrivateKey string // 40字節的 API Key 私鑰（用於簽名交易）
	apiKeyIndex      uint8  // API Key 索引（默認 0）
	accountIndex     int64  // 賬戶索引

	// 認證令牌
	authToken    string
	tokenExpiry  time.Time
	accountMutex sync.RWMutex

	// 市場信息緩存
	symbolPrecision map[string]SymbolPrecision
	precisionMutex  sync.RWMutex

	// 市場索引緩存
	marketIndexMap map[string]uint8 // symbol -> market_id
	marketMutex    sync.RWMutex
}

// NewLighterTraderV2 創建新的 LIGHTER 交易器（使用官方 SDK）
// 參數說明：
//   - l1PrivateKeyHex: L1 錢包私鑰（32字節，標準以太坊私鑰）
//   - walletAddr: 以太坊錢包地址（可選，會從私鑰自動派生）
//   - apiKeyPrivateKeyHex: API Key 私鑰（40字節，用於簽名交易）如果為空則需要生成
//   - testnet: 是否使用測試網
func NewLighterTraderV2(l1PrivateKeyHex, walletAddr, apiKeyPrivateKeyHex string, testnet bool) (*LighterTraderV2, error) {
	// 1. 解析 L1 私鑰
	l1PrivateKeyHex = strings.TrimPrefix(strings.ToLower(l1PrivateKeyHex), "0x")
	l1PrivateKey, err := crypto.HexToECDSA(l1PrivateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("無效的 L1 私鑰: %w", err)
	}

	// 2. 如果沒有提供錢包地址，從私鑰派生
	if walletAddr == "" {
		walletAddr = crypto.PubkeyToAddress(*l1PrivateKey.Public().(*ecdsa.PublicKey)).Hex()
		log.Printf("✓ 從私鑰派生錢包地址: %s", walletAddr)
	}

	// 3. 確定 API URL 和 Chain ID
	baseURL := "https://mainnet.zklighter.elliot.ai"
	chainID := uint32(42766) // Mainnet Chain ID
	if testnet {
		baseURL = "https://testnet.zklighter.elliot.ai"
		chainID = uint32(42069) // Testnet Chain ID
	}

	// 4. 創建 HTTP 客戶端
	httpClient := lighterHTTP.NewClient(baseURL)

	trader := &LighterTraderV2{
		ctx:              context.Background(),
		privateKey:       l1PrivateKey,
		walletAddr:       walletAddr,
		client:           &http.Client{Timeout: 30 * time.Second},
		baseURL:          baseURL,
		testnet:          testnet,
		chainID:          chainID,
		httpClient:       httpClient,
		apiKeyPrivateKey: apiKeyPrivateKeyHex,
		apiKeyIndex:      0, // 默認使用索引 0
		symbolPrecision:  make(map[string]SymbolPrecision),
		marketIndexMap:   make(map[string]uint8),
	}

	// 5. 初始化賬戶（獲取賬戶索引）
	if err := trader.initializeAccount(); err != nil {
		return nil, fmt.Errorf("初始化賬戶失敗: %w", err)
	}

	// 6. 如果沒有 API Key，提示用戶需要生成
	if apiKeyPrivateKeyHex == "" {
		log.Printf("⚠️  未提供 API Key 私鑰，請調用 GenerateAndRegisterAPIKey() 生成")
		log.Printf("   或者從 LIGHTER 官網獲取現有的 API Key")
		return trader, nil
	}

	// 7. 創建 TxClient（用於簽名交易）
	txClient, err := lighterClient.NewTxClient(
		httpClient,
		apiKeyPrivateKeyHex,
		trader.accountIndex,
		trader.apiKeyIndex,
		trader.chainID,
	)
	if err != nil {
		return nil, fmt.Errorf("創建 TxClient 失敗: %w", err)
	}

	trader.txClient = txClient

	// 8. 驗證 API Key 是否正確
	if err := trader.checkClient(); err != nil {
		log.Printf("⚠️  API Key 驗證失敗: %v", err)
		log.Printf("   您可能需要重新生成 API Key 或檢查配置")
		return trader, err
	}

	log.Printf("✓ LIGHTER 交易器初始化成功 (account=%d, apiKey=%d, testnet=%v)",
		trader.accountIndex, trader.apiKeyIndex, testnet)

	return trader, nil
}

// initializeAccount 初始化賬戶信息（獲取賬戶索引）
func (t *LighterTraderV2) initializeAccount() error {
	// 通過 L1 地址獲取賬戶信息
	accountInfo, err := t.getAccountByL1Address()
	if err != nil {
		return fmt.Errorf("獲取賬戶信息失敗: %w", err)
	}

	t.accountMutex.Lock()
	t.accountIndex = accountInfo.AccountIndex
	t.accountMutex.Unlock()

	log.Printf("✓ 賬戶索引: %d", t.accountIndex)
	return nil
}

// getAccountByL1Address 通過 L1 錢包地址獲取 LIGHTER 賬戶信息
func (t *LighterTraderV2) getAccountByL1Address() (*AccountInfo, error) {
	endpoint := fmt.Sprintf("%s/api/v1/account?by=address&value=%s", t.baseURL, t.walletAddr)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("獲取賬戶失敗 (status %d): %s", resp.StatusCode, string(body))
	}

	var accountInfo AccountInfo
	if err := json.Unmarshal(body, &accountInfo); err != nil {
		return nil, fmt.Errorf("解析賬戶響應失敗: %w", err)
	}

	return &accountInfo, nil
}

// checkClient 驗證 API Key 是否正確
func (t *LighterTraderV2) checkClient() error {
	if t.txClient == nil {
		return fmt.Errorf("TxClient 未初始化")
	}

	// 獲取服務器上註冊的 API Key 公鑰
	publicKey, err := t.httpClient.GetApiKey(t.accountIndex, t.apiKeyIndex)
	if err != nil {
		return fmt.Errorf("獲取 API Key 失敗: %w", err)
	}

	// 獲取本地 API Key 的公鑰
	pubKeyBytes := t.txClient.GetKeyManager().PubKeyBytes()
	localPubKey := hexutil.Encode(pubKeyBytes[:])
	localPubKey = strings.Replace(localPubKey, "0x", "", 1)

	// 比對公鑰
	if publicKey != localPubKey {
		return fmt.Errorf("API Key 不匹配：本地=%s, 服務器=%s", localPubKey, publicKey)
	}

	log.Printf("✓ API Key 驗證通過")
	return nil
}

// GenerateAndRegisterAPIKey 生成新的 API Key 並註冊到 LIGHTER
// 注意：這需要 L1 私鑰簽名，所以必須在有 L1 私鑰的情況下調用
func (t *LighterTraderV2) GenerateAndRegisterAPIKey(seed string) (privateKey, publicKey string, err error) {
	// 這個功能需要調用官方 SDK 的 GenerateAPIKey 函數
	// 但這是在 sharedlib 中的 CGO 函數，無法直接在純 Go 代碼中調用
	//
	// 解決方案：
	// 1. 讓用戶從 LIGHTER 官網生成 API Key
	// 2. 或者我們可以實現一個簡單的 API Key 生成包裝器

	return "", "", fmt.Errorf("GenerateAndRegisterAPIKey 功能待實現，請從 LIGHTER 官網生成 API Key")
}

// refreshAuthToken 刷新認證令牌（使用官方 SDK）
func (t *LighterTraderV2) refreshAuthToken() error {
	if t.txClient == nil {
		return fmt.Errorf("TxClient 未初始化，請先設置 API Key")
	}

	// 使用官方 SDK 生成認證令牌（有效期 7 小時）
	deadline := time.Now().Add(7 * time.Hour)
	authToken, err := t.txClient.GetAuthToken(deadline)
	if err != nil {
		return fmt.Errorf("生成認證令牌失敗: %w", err)
	}

	t.accountMutex.Lock()
	t.authToken = authToken
	t.tokenExpiry = deadline
	t.accountMutex.Unlock()

	log.Printf("✓ 認證令牌已生成（有效期至: %s）", t.tokenExpiry.Format(time.RFC3339))
	return nil
}

// ensureAuthToken 確保認證令牌有效
func (t *LighterTraderV2) ensureAuthToken() error {
	t.accountMutex.RLock()
	expired := time.Now().After(t.tokenExpiry.Add(-30 * time.Minute)) // 提前 30 分鐘刷新
	t.accountMutex.RUnlock()

	if expired {
		log.Println("🔄 認證令牌即將過期，刷新中...")
		return t.refreshAuthToken()
	}

	return nil
}

// GetExchangeType 獲取交易所類型
func (t *LighterTraderV2) GetExchangeType() string {
	return "lighter"
}

// Cleanup 清理資源
func (t *LighterTraderV2) Cleanup() error {
	log.Println("⏹  LIGHTER 交易器清理完成")
	return nil
}

func (t *LighterTraderV2) GetHistoryFilledOrders() ([]map[string]interface{}, error) {
	// TODO
	return nil, nil
}
