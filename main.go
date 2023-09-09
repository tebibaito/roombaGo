package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/stianeikeland/go-rpio/v4"
	"github.com/tarm/serial"
)

func main() {
	// gpio初期化
	if err := rpio.Open(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer rpio.Close()

	// シリアル通信初期化
	ser, err := serial.OpenPort((&serial.Config{Name: "/dev/serial0", Baud: 115200, ReadTimeout: time.Second}))
	if err != nil {
		fmt.Println(err)
	}
	defer ser.Close()

	http.HandleFunc("/clean", cleanHandler(ser))
	http.HandleFunc("/dock", dockHandler(ser))
	http.HandleFunc("/battery", getBatteryHandler(ser))
	http.HandleFunc("/poweroff", powerOffHandler(ser))
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func wakeUp(ser *serial.Port) {
	mode := SensorPacket{packetId: 35, dataBytes: 1}
	isCharging := SensorPacket{packetId: 34, dataBytes: 1}
	data, err := readSensor(ser, []SensorPacket{mode, isCharging})
	// fmt.Printf("%v\n", data)
	// エラーが発生したときはOFFモードなので、パルスを流す
	if err != nil {
		pin := rpio.Pin(23)
		pin.Output()
		pin.High()
		time.Sleep(100 * time.Millisecond)
		pin.Low()
		time.Sleep(500 * time.Millisecond)
		pin.High()
		time.Sleep(2 * time.Second)
	}
	// 充電中はスタートコマンドだけでは反応しなのでcleanコマンドを実行
	if data[34] > 0 {
		clean(ser)
		time.Sleep(300 * time.Millisecond)
	} else {
		// スタートコマンド
		start := []byte{128}
		sendCommand(ser, start)
	}
}

func getIsOn(ser *serial.Port) bool {
	mode := SensorPacket{packetId: 35, dataBytes: 1}
	_, err := readSensor(ser, []SensorPacket{mode})
	if err != nil {
		return false
	}
	return true
}

func clean(ser *serial.Port) {
	command := []byte{135}
	sendCommand(ser, command)
	// time.Sleep(500 * time.Millisecond)
	// sendCommand(ser, command)
}

func dock(ser *serial.Port) {
	command := []byte{143}
	sendCommand(ser, command)
}

func powerOff(ser *serial.Port) {
	command := []byte{133}
	sendCommand(ser, command)
}

func powerOffHandler(ser *serial.Port) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		powerOff(ser)
	}
}

func cleanHandler(ser *serial.Port) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isOn := getIsOn(ser); isOn == true {
			powerOff(ser)
			time.Sleep(100 * time.Millisecond)
		}
		wakeUp(ser)
		time.Sleep(100 * time.Millisecond)
		clean(ser)
		fmt.Fprint(w, "start cleaning!")
	}
}

func dockHandler(ser *serial.Port) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isOn := getIsOn(ser); isOn == true {
			powerOff(ser)
			time.Sleep(1000 * time.Millisecond)
		}
		wakeUp(ser)
		time.Sleep(100 * time.Millisecond)
		dock(ser)
		fmt.Fprint(w, "back to homebase!")
	}
}

func sendCommand(ser *serial.Port, command []byte) {
	// time.Sleep(50 * time.Millisecond)
	_, err := ser.Write(command)
	if err != nil {
		fmt.Println(err)
	}
	log.Printf("send %v", command)
}

type SensorPacket struct {
	packetId  byte
	dataBytes int
}

func readSensor(ser *serial.Port, packets []SensorPacket) (map[int]int, error) {
	// 送信用バイト列
	serSequence := []byte{148, byte(len(packets))}
	// key:packetId value:受信バイト数のマップ
	dataBytesMap := map[byte]int{}
	// 合計受信バイト数（初期値の3はスタート、バイト数、チェックサムの３バイト）
	var totalDataBytes int = 3

	for _, p := range packets {
		serSequence = append(serSequence, p.packetId)
		totalDataBytes += (p.dataBytes + 1)
		dataBytesMap[p.packetId] = p.dataBytes
	}
	// 受信回数 一度に8バイトしか受け取れないため
	var receiveCnt int = (totalDataBytes / 8) + 1

	// データ送信
	sendCommand(ser, serSequence)
	time.Sleep(100 * time.Millisecond)

	var received []byte
	for {
		for i := 0; i < receiveCnt; i++ {
			// データ読み取り
			buf := make([]byte, binary.MaxVarintLen64)
			n, err := ser.Read(buf)
			if err != nil {
				return nil, errors.New("serial data receive failed")
			}
			// 正しい順番で受信されるようにする
			if i == 0 && buf[0] == 19 {
				bytes := buf[1]
				received = append(received, buf[:2+bytes+1]...)
			} else {
				if buf[0] != 19 {
					received = append(received, buf[:n]...)
				}
			}
			// fmt.Printf("%v\n", received)
		}
		// チェックサム計算
		var checksum int = 0
		for _, d := range received {
			checksum += int(d)
		}
		if checksum&255<<4 == 0 {
			break
		}
		received = received[:0]

		time.Sleep(5 * time.Millisecond)
	}
	log.Printf("%v\n", received)
	// エンコード
	result := map[int]int{}
	for i := 2; i < len(received)-1; {
		packetId := received[i]
		databytes := dataBytesMap[packetId]
		i += 1
		data := make([]byte, binary.MaxVarintLen32)
		for j := 0; j < databytes; j++ {
			data[j] = received[i+j]
		}
		val := int(binary.LittleEndian.Uint32(data))
		result[int(packetId)] = val
		i += databytes
	}
	log.Printf("%v", result)
	return result, nil
}

type BatteryData struct {
	Charge   int `json:"charge"`
	Capacity int `json:"capacity"`
}

func getBatteryData(ser *serial.Port) BatteryData {
	var batteryData BatteryData
	chargePacket := SensorPacket{packetId: 25, dataBytes: 2}
	capacityPacket := SensorPacket{packetId: 26, dataBytes: 2}
	packets := []SensorPacket{chargePacket, capacityPacket}
	result, err := readSensor(ser, packets)
	if err != nil {
		log.Fatal(err)
		return batteryData
	}
	batteryData.Capacity = result[int(capacityPacket.packetId)]
	batteryData.Charge = result[int(chargePacket.packetId)]
	return batteryData
}

func getBatteryHandler(ser *serial.Port) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// wakeUp(ser)
		batteryData := getBatteryData(ser)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		if err := enc.Encode(&batteryData); err != nil {
			log.Fatal(err)
		}
		fmt.Println(buf.String())

		_, err := fmt.Fprint(w, buf.String())
		if err != nil {
			return
		}
	}
}
