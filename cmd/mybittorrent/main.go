package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"unicode"

	bencode "github.com/jackpal/bencode-go"
)

type torrentFile struct {
	Announce string
	Length int
	InfoHash string
	PieceLength int
	Pieces []string
}

func decodeBencode(bencodedString string, index int) (interface{}, int, error) {
	if unicode.IsDigit(rune(bencodedString[index])) {
		var firstColonIndex int

		for i := index + 1; i < len(bencodedString); i++ {
			if bencodedString[i] == ':' {
				firstColonIndex = i
				break
			}
		}

		lengthStr := bencodedString[index:firstColonIndex]

		length, err := strconv.Atoi(lengthStr)
		if err != nil {
			return "", 0, err
		}

		return bencodedString[firstColonIndex+1 : firstColonIndex+1+length], firstColonIndex+1+length, nil
	} else if bencodedString[index] == 'i'{

		for i := index + 1; i < len(bencodedString); i++ {
			if bencodedString[i] == 'e' {
				intValue, err := strconv.Atoi(bencodedString[index + 1:i])
				if err != nil {
					return nil, 0, err
				}
				return intValue, i + 1, nil
			}
		}

	} else if bencodedString[index] == 'l' {
		var decodedList []interface{} = []interface{}{}
		i := index + 1

		for {
			if bencodedString[i] == 'e' {
				return decodedList, i + 1, nil
			}
			decodedValue, newIndex, err := decodeBencode(bencodedString, i)
			if err != nil {
				return nil, 0, err
			}
			decodedList = append(decodedList, decodedValue)
			i = newIndex
		}
	} else if bencodedString[index] == 'd' {
		var decodeDict map[string]interface{} = map[string]interface{}{}
		i := index + 1

		for {
			if bencodedString[i] == 'e' {
				return decodeDict, i + 1, nil
			}

			decodeDictKey, newIndex, err := decodeBencode(bencodedString, i)
			if err != nil {
				return nil, 0, err
			}

			decodeDictValue, newIndex, err := decodeBencode(bencodedString, newIndex)
			if err != nil {
				return nil, 0, err
			}

			decodeDict[decodeDictKey.(string)] = decodeDictValue
			i = newIndex
		}
	}
	return nil, 0, nil
}

func parseTorrentFile(filePath string) (map[string]interface{}, error) {
	file, err := os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
		decoded, _, err := decodeBencode(string(file), 0)
		if err != nil {
			return nil, err
		}
		// jsonOutput, _ := json.Marshal(decoded)
		// fmt.Println(string(jsonOutput))
		
		return decoded.(map[string]interface{}), err
}

func getInfo(filePath string, torrentFile *torrentFile)  {

		torrentMeta, _ := parseTorrentFile(filePath)
		
		infoMap := torrentMeta["info"].(map[string]interface{})
		h := sha1.New()
		bencode.Marshal(h, infoMap)
		torrentFile.Announce = torrentMeta["announce"].(string)
		torrentFile.InfoHash = string(h.Sum(nil))
		torrentFile.Length = infoMap["length"].(int)
		torrentFile.PieceLength = infoMap["piece length"].(int)

		pieces := infoMap["pieces"].(string)
		for i := 0; i < len(pieces); i += 20 {
			torrentFile.Pieces = append(torrentFile.Pieces, pieces[i:i+20])
		}
		
}

func handshake(torrentFile torrentFile, peerIp string) (net.Conn, error) {

	conn, err := net.Dial("tcp", peerIp)
	if err != nil {
		return nil, err
	}


	pstr := []byte("BitTorrent protocol")
	handshake := append([]byte{19}, pstr...)
	reserv := make([]byte, 8)
	handshake = append(handshake, reserv...)
	handshake = append(handshake, torrentFile.InfoHash...)
	handshake = append(handshake, []byte("12345678901234567890")...)

	conn.Write(handshake)

	return conn, nil

}

func getPeers(torrentFile torrentFile) ([] string, error ){

	baseURL := torrentFile.Announce
	queryParams := url.Values{}
	queryParams.Add("info_hash", torrentFile.InfoHash)
	queryParams.Add("peer_id", "12345678901234567890")
	queryParams.Add("left", strconv.Itoa(torrentFile.Length))
	queryParams.Add("downloaded", "0")
	queryParams.Add("uploaded", "0")
	queryParams.Add("port", "6881")
	queryParams.Add("compact", "0")

	finalURL := baseURL + "?" + queryParams.Encode()

	response, err := http.Get(finalURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	decoded, _, _ := decodeBencode(string(body), 0)

	peers := decoded.(map[string]interface{})
	peerList := peers["peers"].([]interface{})

	var peersOut []string

	for _, peer := range peerList {
		peerMap := peer.(map[string]interface{})
		peerIp := peerMap["ip"].(string)
		peerPort := peerMap["port"].(int)
		peerStr := fmt.Sprintf("[%s]:%v", peerIp, peerPort)
		peersOut = append(peersOut, peerStr)
	}

	return peersOut, nil		
}

func downloadPiece(conn net.Conn, torrentFile torrentFile, index int) ([]byte, error){

	// Break the piece into blocks of 16 kiB (16 * 1024 bytes) and send a request message for each block

	type Message struct {
		lengthPrefix	uint32
		id		uint8
		index	uint32
		begin	uint32
		length	uint32
	}

	blockSize := 16 * 1024
	pieceSize := torrentFile.PieceLength
	totalPieces := int(math.Ceil(float64(torrentFile.Length) / float64(pieceSize)))
	if index == totalPieces - 1 {
		pieceSize = torrentFile.Length % torrentFile.PieceLength
	}
	totalBlocks :=  int(math.Ceil(float64(pieceSize) / float64(blockSize)))

	var data []byte

	for i := 0; i < totalBlocks; i++ {
		blockLen := blockSize
		if int(totalBlocks) == i + 1 {
			blockLen = pieceSize - (i * blockSize)
		}

		message := Message {
			lengthPrefix: 13,
			id: 6,
			index: uint32(index),
			begin: uint32(i * blockSize),
			length: uint32(blockLen),
		}

		var buffer bytes.Buffer
		binary.Write(&buffer, binary.BigEndian, message)
		conn.Write(buffer.Bytes())

		// Wait for a piece message for each block you've requested

		buff := make([]byte, 4)
		conn.Read(buff)

		message = Message{}
		message.lengthPrefix = binary.BigEndian.Uint32(buff)

		buff = make([]byte, message.lengthPrefix)
		// n, _ := conn.Read(buff)
		// n, _ := io.ReadFull(conn, buff)
		io.ReadFull(conn, buff)
		message.id = buff[0]

		data = append(data, buff[9:]...)
	}	
	return data, nil
	
}


func main() {
	command := os.Args[1]
	
	if command == "decode" {
		bencodedValue := os.Args[2]
		
		decoded, _, err := decodeBencode(bencodedValue, 0)
		if err != nil {
			fmt.Println(err)
			return
		}
		
		jsonOutput, _ := json.Marshal(decoded)
		fmt.Println(string(jsonOutput))
	} else if command == "info" {
		filePath := os.Args[2]
		torrentMeta, err := parseTorrentFile(filePath)

		if err != nil {
			fmt.Println(err)
			return
		}

		infoMap := torrentMeta["info"].(map[string]interface{})

		h := sha1.New()
		bencode.Marshal(h, infoMap)
		fmt.Printf("Tracker URL: %s\n", torrentMeta["announce"])
		fmt.Printf("Length: %v\n", infoMap["length"])
		fmt.Printf("Info Hash: %x\n", h.Sum(nil))
		fmt.Printf("Piece Length: %v\n", infoMap["piece length"])
		fmt.Printf("Piece Hashes: \n", )

		pieces := infoMap["pieces"].(string)

		for i := 0; i < len(pieces); i += 20 {
			fmt.Printf("%x\n", pieces[i:i+20])
		}

		
	} else if command == "peers" {
		var torrentFile torrentFile

		getInfo(os.Args[2], &torrentFile)

		baseURL := torrentFile.Announce
        queryParams := url.Values{}
        queryParams.Add("info_hash", torrentFile.InfoHash)
        queryParams.Add("peer_id", "12345678901234567890")
        queryParams.Add("left", strconv.Itoa(torrentFile.Length))
        queryParams.Add("downloaded", "0")
        queryParams.Add("uploaded", "0")
		queryParams.Add("port", "6881")
		queryParams.Add("compact", "0")

        finalURL := baseURL + "?" + queryParams.Encode()

		response, err := http.Get(finalURL)
		if err != nil {
			fmt.Println("Error making HTTP request:", err)
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		decoded, _, _ := decodeBencode(string(body), 0)

		peers := decoded.(map[string]interface{})
		peerList := peers["peers"].([]interface{})

		for _, peer := range peerList {
			peerMap := peer.(map[string]interface{})
			peerIp := peerMap["ip"].(string)
			peerPort := peerMap["port"].(int)
			fmt.Printf("[%s]:%v", peerIp, peerPort)
		}

	} else if command == "handshake"{
		var torrentFile torrentFile

		getInfo(os.Args[2], &torrentFile)

		conn, err := net.Dial("tcp", os.Args[3])
		if err != nil {
			fmt.Println(err)
			return
		}

		defer conn.Close()

		pstr := []byte("BitTorrent protocol")
		handshake := append([]byte{19}, pstr...)
		reserv := make([]byte, 8)
		handshake = append(handshake, reserv...)
		handshake = append(handshake, torrentFile.InfoHash...)
		handshake = append(handshake, []byte("12345678901234567890")...)

		conn.Write(handshake)
		buf := make([]byte, 68)
		_, err = conn.Read(buf)
		if err != nil {
			fmt.Println("failed:", err)
			return
		}
		fmt.Printf("Peer ID: %s\n", hex.EncodeToString(buf[48:]))

		return 
		

	} else if command == "download_piece" {
		var torrentFile torrentFile

		getInfo(os.Args[4], &torrentFile)
		peerList, _ := getPeers(torrentFile)

		conn, err := handshake(torrentFile, peerList[2])
		if err != nil {
			fmt.Println(err)
			return
		}
		defer conn.Close()

		buff := make([]byte, 68)
		conn.Read(buff)

		// fmt.Printf("%v", buff)

		index , _ := strconv.Atoi(os.Args[5])

		data, _ := downloadPiece(conn, torrentFile,index)

		hash := sha1.New()
		hash.Write(data)

		fmt.Printf("%x\n", hash.Sum(nil))

		fmt.Printf("%x\n", torrentFile.Pieces[1])

		

	} else if command == "download" {
		var torrentFile torrentFile

		getInfo(os.Args[4], &torrentFile)
		peerList, _ := getPeers(torrentFile)

		conn, err := handshake(torrentFile, peerList[0])
		if err != nil {
			fmt.Println(err)
			return
		}
		defer conn.Close()

		buff := make([]byte, 68)
		conn.Read(buff)

		// Wait for a bitfield message

		buff = make([]byte, 4)
		conn.Read(buff)

		msgLen := binary.BigEndian.Uint32(buff)

		buff = make([]byte, msgLen)
		conn.Read(buff)
		msgId := buff[0]

		if msgId != 5 {
			fmt.Printf("Invalid bitfield")
			return
		}

		// Send an interested message

		conn.Write([]byte{0, 0, 0, 1, 2})

		// Wait until you receive an unchoke message

		buff = make([]byte, 4)
		conn.Read(buff)
		msgLen = binary.BigEndian.Uint32(buff)

		buff = make([]byte, msgLen)
		conn.Read(buff)
		msgId = buff[0]

		if msgId != 1 {
			fmt.Printf("Invalid unchoke")
			return
		}

		// Download all the pieces
		
		fileLocation := os.Args[3]
		totalIndex := len(torrentFile.Pieces)

		file, err := os.OpenFile(fileLocation, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Println("Error opening or creating the file:", err)
			return
		}
		defer file.Close()

		for i := 0; i < totalIndex; i++ {
			data, err := downloadPiece(conn, torrentFile, i)
			if err != nil {
				fmt.Printf("Error downloading piece %d: %v\n", i, err)
				return
			}
			hash := sha1.New()
			hash.Write(data)

			fmt.Printf("%x %x \n", torrentFile.Pieces[i], hash.Sum(nil))

			_, err = file.Write(data)
			if err != nil {
				fmt.Println("Error writing to file:", err)
			}
		}
		fileInfo, err := os.Stat(fileLocation)
		if err != nil {
			fmt.Println("Error getting file info:", err)
			return
		}

		fileSize := fileInfo.Size()
		fmt.Printf("File size: %d bytes, Downloaded %v bytes\n", fileSize, torrentFile.Length)
		return

	} else {
		fmt.Println("Unknown command: " + command)
		os.Exit(1)
	}
}