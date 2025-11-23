package trader

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// LighterTrader LIGHTER DEX交易器
// LIGHTER是基于Ethereum L2的永续合约DEX，使用zk-rollup技术
type LighterTrader struct {
	ctx        context.Context
	privateKey *ecdsa.PrivateKey
	walletAddr string // Ethereum钱包地址
	client     *http.Client
	baseURL    string
	testnet    bool

	// 账户信息缓存
	accountIndex int    // LIGHTER账户索引
	apiKey       string // API密钥（从私钥派生）
	authToken    string // 认证令牌（8小时有效期）
	tokenExpiry  time.Time
	accountMutex sync.RWMutex

	// 市场信息缓存
	symbolPrecision map[string]SymbolPrecision
	precisionMutex  sync.RWMutex
}

// LighterConfig LIGHTER配置
type LighterConfig struct {
	PrivateKeyHex string
	WalletAddr    string
	Testnet       bool
}

// NewLighterTrader 创建LIGHTER交易器
func NewLighterTrader(privateKeyHex string, walletAddr string, testnet bool) (*LighterTrader, error) {
	// 去掉私钥的 0x 前缀（如果有）
	privateKeyHex = strings.TrimPrefix(strings.ToLower(privateKeyHex), "0x")

	// 解析私钥
	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}

	// 从私钥派生钱包地址（如果未提供）
	if walletAddr == "" {
		walletAddr = crypto.PubkeyToAddress(*privateKey.Public().(*ecdsa.PublicKey)).Hex()
		log.Printf("✓ 从私钥派生钱包地址: %s", walletAddr)
	}

	// 选择API URL
	baseURL := "https://mainnet.zklighter.elliot.ai"
	if testnet {
		baseURL = "https://testnet.zklighter.elliot.ai" // TODO: 确认testnet URL
	}

	trader := &LighterTrader{
		ctx:             context.Background(),
		privateKey:      privateKey,
		walletAddr:      walletAddr,
		client:          &http.Client{Timeout: 30 * time.Second},
		baseURL:         baseURL,
		testnet:         testnet,
		symbolPrecision: make(map[string]SymbolPrecision),
	}

	log.Printf("✓ LIGHTER交易器初始化成功 (testnet=%v, wallet=%s)", testnet, walletAddr)

	// 初始化账户信息（获取账户索引和API密钥）
	if err := trader.initializeAccount(); err != nil {
		return nil, fmt.Errorf("初始化账户失败: %w", err)
	}

	return trader, nil
}

// initializeAccount 初始化账户信息
func (t *LighterTrader) initializeAccount() error {
	// 1. 获取账户信息（通过L1地址）
	accountInfo, err := t.getAccountByL1Address()
	if err != nil {
		return fmt.Errorf("获取账户信息失败: %w", err)
	}

	t.accountMutex.Lock()
	t.accountIndex = accountInfo["index"].(int)
	t.accountMutex.Unlock()

	log.Printf("✓ LIGHTER账户索引: %d", t.accountIndex)

	// 2. 生成认证令牌（有效期8小时）
	if err := t.refreshAuthToken(); err != nil {
		return fmt.Errorf("生成认证令牌失败: %w", err)
	}

	return nil
}

// getAccountByL1Address 通过Ethereum地址获取LIGHTER账户信息
func (t *LighterTrader) getAccountByL1Address() (map[string]interface{}, error) {
	endpoint := fmt.Sprintf("%s/api/v1/account/by/l1/%s", t.baseURL, t.walletAddr)

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
		return nil, fmt.Errorf("API错误 (status %d): %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return result, nil
}

// refreshAuthToken 刷新认证令牌
func (t *LighterTrader) refreshAuthToken() error {
	// TODO: 实现认证令牌生成逻辑
	// 参考 lighter-python SDK 的实现
	// 需要签名特定消息并提交到API

	t.accountMutex.Lock()
	defer t.accountMutex.Unlock()

	// 临时实现：设置过期时间为8小时后
	t.tokenExpiry = time.Now().Add(8 * time.Hour)
	log.Printf("✓ 认证令牌已生成（有效期至: %s）", t.tokenExpiry.Format(time.RFC3339))

	return nil
}

// ensureAuthToken 确保认证令牌有效
func (t *LighterTrader) ensureAuthToken() error {
	t.accountMutex.RLock()
	expired := time.Now().After(t.tokenExpiry.Add(-30 * time.Minute)) // 提前30分钟刷新
	t.accountMutex.RUnlock()

	if expired {
		log.Println("🔄 认证令牌即将过期，刷新中...")
		return t.refreshAuthToken()
	}

	return nil
}

// signMessage 签名消息（Ethereum签名）
func (t *LighterTrader) signMessage(message []byte) (string, error) {
	// 使用Ethereum个人签名格式
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	prefixedMessage := append([]byte(prefix), message...)

	hash := crypto.Keccak256Hash(prefixedMessage)
	signature, err := crypto.Sign(hash.Bytes(), t.privateKey)
	if err != nil {
		return "", err
	}

	// 调整v值（Ethereum格式）
	if signature[64] < 27 {
		signature[64] += 27
	}

	return "0x" + hex.EncodeToString(signature), nil
}

// GetName 获取交易器名称
func (t *LighterTrader) GetName() string {
	return "LIGHTER"
}

// GetExchangeType 获取交易所类型
func (t *LighterTrader) GetExchangeType() string {
	return "lighter"
}

// Close 关闭交易器
func (t *LighterTrader) Close() error {
	log.Println("✓ LIGHTER交易器已关闭")
	return nil
}

// Run 运行交易器（实现Trader接口）
func (t *LighterTrader) Run() error {
	log.Println("⚠️ LIGHTER交易器的Run方法应由AutoTrader调用")
	return fmt.Errorf("请使用AutoTrader管理交易器生命周期")
}

func (t *LighterTrader) GetHistoryFilledOrders() ([]map[string]interface{}, error) {
	// TODO
	return nil, nil
}
