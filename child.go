package main

import (
	"bufio"
	"encoding/binary" // For length prefix
	"flag"            // Added for command-line arguments

	"fmt"
	"io"
	"log"
	"os"
	"sync"

	ipcgen "ipc/ipcgen" // Assuming generated FlatBuffer code

	agoraservice "github.com/AgoraIO-Extensions/Agora-Golang-Server-SDK/v2/go_sdk/agoraservice"
	flatbuffers "github.com/google/flatbuffers/go"
)

var (
	childLogger  *log.Logger
	stdoutWriter *bufio.Writer // To allow observer to send messages
	stdoutLock   sync.Mutex    // To protect writes to stdoutWriter from multiple goroutines (main + observer)

	// Global Agora SDK objects - to be properly managed
	mediaFactory    *agoraservice.MediaNodeFactory
	videoSender     *agoraservice.VideoFrameSender
	audioSender     *agoraservice.AudioPcmDataSender
	localVideoTrack *agoraservice.LocalVideoTrack
	localAudioTrack *agoraservice.LocalAudioTrack

	rtcConnection     *agoraservice.RtcConnection
	initWidth         int32
	initHeight        int32
	initFrameRate     int32
	initVideoCodec    agoraservice.VideoCodecType
	initSampleRate    int32
	initAudioChannels int32
	initBitrate       int // Added for command-line bitrate
	initMinBitrate    int // Added for command-line min bitrate

	globalAppID   string // Stores AppID from InitCommand
	globalChannel string // Stores ChannelName from InitCommand
	globalUserID  string // Stores UserID from InitCommand
)

// Callback implementations for RtcConnectionObserver
// These are standalone functions now, not methods of a struct.

func onConnected(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, reason int) {
	logMsg := fmt.Sprintf("Agora SDK: Connected. UserID: %s, Channel: %s, Reason: %d", conInfo.LocalUserId, conInfo.ChannelId, reason)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelInfo, logMsg)

	if err := setupMediaInfrastructureAndPublish(conn); err != nil {
		errMsg := fmt.Sprintf("Failed to setup media infrastructure: %v", err)
		childLogger.Println("ERROR: " + errMsg)
		sendAsyncErrorResponse(ipcgen.ConnectionStatusFailed, errMsg, "MediaSetupError")
	} else {
		sendAsyncStatusResponse(ipcgen.ConnectionStatusConnected, "Successfully connected and media infrastructure prepared.", "")
	}
}

func onDisconnected(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, reason int) {
	logMsg := fmt.Sprintf("Agora SDK: Disconnected. Reason: %d", reason)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelWarn, logMsg)
	sendAsyncStatusResponse(ipcgen.ConnectionStatusDisconnected, logMsg, "")
	cleanupLocalRtcResources(false) // Don't release the connection object itself, just tracks etc.
}

func onReconnecting(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, reason int) {
	logMsg := fmt.Sprintf("Agora SDK: Reconnecting... Reason: %d", reason)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelInfo, logMsg)
	sendAsyncStatusResponse(ipcgen.ConnectionStatusReconnecting, logMsg, "")
}

func onReconnected(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, reason int) {
	logMsg := fmt.Sprintf("Agora SDK: Reconnected. UserID: %s, Channel: %s, Reason: %d", conInfo.LocalUserId, conInfo.ChannelId, reason)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelInfo, logMsg)
	sendAsyncStatusResponse(ipcgen.ConnectionStatusReconnected, "Successfully reconnected.", "")
}

func onConnectionLost(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo) {
	logMsg := fmt.Sprintf("Agora SDK: Connection lost. UserID: %s, Channel: %s", conInfo.LocalUserId, conInfo.ChannelId)
	childLogger.Println("ERROR: " + logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelError, logMsg)
	sendAsyncStatusResponse(ipcgen.ConnectionStatusConnectionLost, logMsg, "")
	cleanupLocalRtcResources(false)
}

func onConnectionFailure(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, errCode int) {
	logMsg := fmt.Sprintf("Agora SDK: Connection failure. Error Code: %d", errCode)
	childLogger.Println("ERROR: " + logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelError, logMsg)
	sendAsyncErrorResponse(ipcgen.ConnectionStatusFailed, logMsg, fmt.Sprintf("AgoraErrorCode: %d", errCode))
	cleanupLocalRtcResources(false)
}

func onUserJoined(conn *agoraservice.RtcConnection, uid string) {
	logMsg := fmt.Sprintf("Agora SDK: User %s joined", uid)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelInfo, logMsg)
}

func onUserLeft(conn *agoraservice.RtcConnection, uid string, reason int) {
	logMsg := fmt.Sprintf("Agora SDK: User %s left. Reason: %d", uid, reason)
	childLogger.Println(logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelInfo, logMsg)
}

func onError(conn *agoraservice.RtcConnection, err int, msg string) {
	logMsg := fmt.Sprintf("Agora SDK: Error. Code: %d, Message: %s", err, msg)
	childLogger.Println("ERROR: " + logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelError, logMsg)
	// This is a general SDK error. May not always map to a connection status.
	// Sending a log is good. If it implies a critical state, could send a Failed status.
}

func onTokenPrivilegeWillExpire(conn *agoraservice.RtcConnection, token string) {
	logMsg := "Agora SDK: Token privilege will expire soon. New token required."
	childLogger.Println("WARN: " + logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelWarn, logMsg)
	sendAsyncStatusResponse(ipcgen.ConnectionStatusTokenWillExpire, "Token privilege will expire.", token) // Send expiring token in details
}

func onTokenPrivilegeDidExpire(conn *agoraservice.RtcConnection) {
	logMsg := "Agora SDK: Token privilege did expire."
	childLogger.Println("WARN: " + logMsg)
	sendAsyncLogResponse(ipcgen.LogLevelWarn, logMsg)
	// Using Failed status as there's no specific "TokenDidExpire" in current FBS ConnectionStatus
	sendAsyncStatusResponse(ipcgen.ConnectionStatusFailed, "Token privilege did expire.", "Token_Expired_Detail")
}

// cleanupLocalRtcResources is for cleaning up tracks or other resources tied to a connection
// without releasing the rtcConnection object itself unless specified.
func cleanupLocalRtcResources(releaseConnectionObject bool) {
	childLogger.Println("Cleaning up local Agora RTC resources...")
	if rtcConnection != nil {
		localUser := rtcConnection.GetLocalUser()
		if localUser != nil {
			if localVideoTrack != nil {
				childLogger.Println("Unpublishing video track...")
				localUser.UnpublishVideo(localVideoTrack)
				childLogger.Println("Releasing local video track...")
				localVideoTrack.Release()
				localVideoTrack = nil
			}
			if localAudioTrack != nil {
				childLogger.Println("Unpublishing audio track...")
				localUser.UnpublishAudio(localAudioTrack)
				childLogger.Println("Releasing local audio track...")
				localAudioTrack.Release()
				localAudioTrack = nil
			}
			localUser.UnsubscribeAllAudio()
			localUser.UnsubscribeAllVideo()
		}
	}

	if videoSender != nil {
		childLogger.Println("Releasing video sender...")
		videoSender.Release()
		videoSender = nil
	}
	if audioSender != nil {
		childLogger.Println("Releasing audio sender...")
		audioSender.Release()
		audioSender = nil
	}

	// rtcConnection cleanup is handled after this block based on releaseConnectionObject
	if rtcConnection != nil { // Added this check for safety before accessing rtcConnection
		if releaseConnectionObject {
			childLogger.Println("Disconnecting and Releasing RtcConnection object...")
			rtcConnection.Disconnect() // Ensure Disconnect is called before Release
			rtcConnection.Release()
			rtcConnection = nil
		} else {
			childLogger.Println("Disconnecting RtcConnection (but not releasing object)...")
			rtcConnection.Disconnect()
		}
	}
	childLogger.Println("Local Agora RTC resources cleanup attempt finished.")
}

func main() {
	childLogger = log.New(os.Stderr, "[agora_worker] ", log.LstdFlags|log.Lshortfile)
	childLogger.Println("Agora child process started.")
	stdoutWriter = bufio.NewWriter(os.Stdout) // Initialize stdoutWriter as *bufio.Writer

	// Define command-line flags
	appIDFlag := flag.String("appID", "", "Agora App ID")
	channelNameFlag := flag.String("channelName", "", "Agora Channel Name")
	userIDFlag := flag.String("userID", "", "Agora User ID for the child process")
	tokenFlag := flag.String("token", "", "Agora Token for the child process")
	widthFlag := flag.Int("width", 1280, "Video width")
	heightFlag := flag.Int("height", 720, "Video height")
	frameRateFlag := flag.Int("frameRate", 30, "Video frame rate")
	videoCodecFlag := flag.String("videoCodec", "H264", "Video codec (H264 or VP8)")
	sampleRateFlag := flag.Int("sampleRate", 48000, "Audio sample rate")
	audioChannelsFlag := flag.Int("audioChannels", 1, "Audio channels")
	bitrateFlag := flag.Int("bitrate", 1000, "Video target bitrate in Kbps")
	minBitrateFlag := flag.Int("minBitrate", 100, "Video minimum bitrate in Kbps")
	// To-Do: Add log level flag if needed: logLevelFlag := flag.String("logLevel", "info", "Logging level")

	flag.Parse() // Parse the flags

	globalAppID = *appIDFlag
	globalChannel = *channelNameFlag
	globalUserID = *userIDFlag
	childProcessToken := *tokenFlag // Store token locally for connect
	initWidth = int32(*widthFlag)
	initHeight = int32(*heightFlag)
	initFrameRate = int32(*frameRateFlag)
	initSampleRate = int32(*sampleRateFlag)
	initAudioChannels = int32(*audioChannelsFlag)
	initBitrate = *bitrateFlag
	initMinBitrate = *minBitrateFlag

	childLogger.Printf("Initial parameters from command line: AppID=%s, Channel=%s, UserID=%s, Codec=%s, Res=%dx%d@%d, Bitrate=%dKbps, MinBitrate=%dKbps, AudioSR=%d, AudioCh=%d",
		globalAppID, globalChannel, globalUserID, *videoCodecFlag, initWidth, initHeight, initFrameRate, initBitrate, initMinBitrate, initSampleRate, initAudioChannels)

	serviceCfg := agoraservice.NewAgoraServiceConfig()
	serviceCfg.EnableAudioProcessor = true
	serviceCfg.EnableVideo = true
	serviceCfg.AppId = globalAppID // Use AppID from flags for SDK initialization
	serviceCfg.LogPath = "./agora_child_sdk.log"
	serviceCfg.LogSize = 5 * 1024 * 1024 // 5 MB

	if ret := agoraservice.Initialize(serviceCfg); ret != 0 {
		errMsg := fmt.Sprintf("Agora SDK global Initialize() failed with code: %d", ret)
		childLogger.Println("FATAL: " + errMsg)
		sendErrorResponse(ipcgen.ConnectionStatusInitializedFailure, errMsg, "GlobalInitializeFailed")
		os.Exit(1)
	}
	childLogger.Println("Agora SDK global Initialize() successful.")
	defer agoraservice.Release()

	mediaFactory = agoraservice.NewMediaNodeFactory()
	if mediaFactory == nil {
		childLogger.Println("FATAL: Failed to create MediaNodeFactory")
		sendErrorResponse(ipcgen.ConnectionStatusInitializedFailure, "Failed to create MediaNodeFactory", "")
		os.Exit(1)
	}
	childLogger.Println("MediaNodeFactory created.")
	// defer mediaFactory.Release() // Typically released with service

	// Determine video codec type from flags
	switch *videoCodecFlag {
	case "H264":
		initVideoCodec = agoraservice.VideoCodecTypeH264
	case "VP8":
		initVideoCodec = agoraservice.VideoCodecTypeVp8
	default:
		childLogger.Printf("WARN: Unsupported video_codec_name '%s' from CLI, defaulting to H264 for Agora.", *videoCodecFlag)
		initVideoCodec = agoraservice.VideoCodecTypeH264
	}

	connCfg := &agoraservice.RtcConnectionConfig{
		AutoSubscribeAudio: false,
		AutoSubscribeVideo: false,
		ClientRole:         agoraservice.ClientRoleBroadcaster,          // Set if needed
		ChannelProfile:     agoraservice.ChannelProfileLiveBroadcasting, // Set if needed
	}

	rtcConnection = agoraservice.NewRtcConnection(connCfg)
	if rtcConnection == nil {
		errMsg := "Failed to create Agora RtcConnection instance."
		childLogger.Println("ERROR: " + errMsg)
		sendErrorResponse(ipcgen.ConnectionStatusInitializedFailure, errMsg, "NewRtcConnectionFailed")
		os.Exit(1) // Exit as we can't proceed
	}

	observer := &agoraservice.RtcConnectionObserver{
		OnConnected:    appOnConnected,
		OnDisconnected: appOnDisconnected,
		OnConnecting: func(conn *agoraservice.RtcConnection, conInfo *agoraservice.RtcConnectionInfo, reason int) {
			logMsg := fmt.Sprintf("Agora SDK: Connecting... UserID: %s, Channel: %s, Reason: %d", conInfo.LocalUserId, conInfo.ChannelId, reason)
			childLogger.Println(logMsg)
			sendAsyncLogResponse(ipcgen.LogLevelInfo, "Connecting...")
			// sendAsyncStatusResponse(ipcgen.ConnectionStatusReconnecting, "Connecting...", fmt.Sprintf("Reason: %d", reason)) // This status is more for reconnecting
		},
		OnReconnecting:             appOnReconnecting,
		OnReconnected:              appOnReconnected,
		OnConnectionLost:           appOnConnectionLost,
		OnConnectionFailure:        appOnConnectionFailure,
		OnTokenPrivilegeWillExpire: appOnTokenPrivilegeWillExpire,
		OnTokenPrivilegeDidExpire:  appOnTokenPrivilegeDidExpire,
		OnUserJoined:               appOnUserJoined,
		OnUserLeft:                 appOnUserLeft,
		OnError:                    appOnError,
	}
	if ret := rtcConnection.RegisterObserver(observer); ret != 0 {
		errMsg := fmt.Sprintf("Failed to register RtcConnectionObserver, error code: %d", ret)
		childLogger.Println("ERROR: " + errMsg)
		sendErrorResponse(ipcgen.ConnectionStatusInitializedFailure, errMsg, "RegisterObserverFailed")
		rtcConnection.Release() // Clean up connection object
		rtcConnection = nil
		os.Exit(1) // Exit as we can't proceed
	}
	childLogger.Println("Agora RtcConnection created and observer registered.")

	ret := rtcConnection.Connect(childProcessToken, globalChannel, globalUserID)
	if ret != 0 {
		errMsg := fmt.Sprintf("Agora RtcConnection.Connect() call failed with code: %d", ret)
		childLogger.Println("ERROR: " + errMsg)
		rtcConnection.UnregisterObserver()
		rtcConnection.Release()
		rtcConnection = nil
		sendErrorResponse(ipcgen.ConnectionStatusInitializedFailure, errMsg, "ConnectFailed")
		os.Exit(1) // Exit as we can't proceed
	}
	childLogger.Printf("Agora RtcConnection.Connect() called for channel '%s', user '%s'. Waiting for connection callbacks.", globalChannel, globalUserID)
	// Send an initial status to parent indicating connection attempt has started.
	sendStatusResponse(ipcgen.ConnectionStatusInitializedSuccess, "Connect call issued, awaiting callback.", "")

	reader := bufio.NewReader(os.Stdin)

	for {
		// 1. Read 4-byte length prefix
		lenBytes := make([]byte, 4)
		if _, err := io.ReadFull(reader, lenBytes); err != nil {
			if err == io.EOF {
				childLogger.Println("Stdin closed, parent process likely terminated. Exiting.")
			} else {
				childLogger.Printf("Error reading message length from stdin: %v. Exiting.", err)
			}
			return
		}
		msgLen := binary.BigEndian.Uint32(lenBytes)

		if msgLen == 0 {
			childLogger.Println("Received 0-length message, skipping.")
			continue
		}

		// 2. Read the message payload
		msgBuf := make([]byte, msgLen)
		if _, err := io.ReadFull(reader, msgBuf); err != nil {
			childLogger.Printf("Error reading message payload (len %d) from stdin: %v. Exiting.", msgLen, err)
			// Maybe send a failure response if the pipe is still writable?
			return
		}

		// 3. Parse FlatBuffer message
		ipcMsg := ipcgen.GetRootAsIPCMessage(msgBuf, 0)
		payloadTable := new(flatbuffers.Table)
		if !ipcMsg.Payload(payloadTable) && ipcMsg.MessageType() != ipcgen.MessageTypeCLOSE_COMMAND {
			// CLOSE_COMMAND might not have a payload, so allow it to proceed.
			childLogger.Printf("Failed to get payload table for message type: %s", ipcgen.EnumNamesMessageType[ipcMsg.MessageType()])
			sendErrorResponse(ipcgen.ConnectionStatusFailed, "Failed to get payload table from IPCMessage", "")
			continue
		}

		// childLogger.Printf("Received command: %s", ipcgen.EnumNamesMessageType[ipcMsg.MessageType()])

		switch ipcMsg.MessageType() {
		// Removed MessageTypeINIT_COMMAND case
		// Connection is now handled at startup

		case ipcgen.MessageTypeWRITE_VIDEO_SAMPLE_COMMAND:
			if rtcConnection == nil || videoSender == nil { // Check rtcConnection not connection (old var)
				childLogger.Println("WARN: Video sample received but Agora rtcConnection/video sender not ready. Dropping.")
				continue
			}
			samplePayload := new(ipcgen.MediaSamplePayload)
			samplePayload.Init(payloadTable.Bytes, payloadTable.Pos)
			if samplePayload.DataLength() == 0 {
				// childLogger.Println("WARN: Received empty video sample data.")
				continue
			}
			frameData := samplePayload.DataBytes() // This is a zero-copy read from the buffer
			// Agora expects timestamp in milliseconds for ExternalVideoFrame.
			// samplePayload.TimestampUnixNano() is in nanoseconds.
			// timestampMs := samplePayload.TimestampUnixNano() / 1e6

			// Use initWidth, initHeight, initAudioChannels, initSampleRate which are now set from flags
			extFrame := &agoraservice.ExternalVideoFrame{
				Type:      agoraservice.VideoBufferRawData,
				Format:    agoraservice.VideoPixelI420,
				Buffer:    frameData,
				Stride:    int(initWidth),
				Height:    int(initHeight),
				Timestamp: int64(0),
			}
			if ret := videoSender.SendVideoFrame(extFrame); ret != 0 {
				childLogger.Printf("WARN: videoSender.SendVideoFrame failed, error code: %d", ret)
			}

		case ipcgen.MessageTypeWRITE_AUDIO_SAMPLE_COMMAND:
			if rtcConnection == nil || audioSender == nil { // Check rtcConnection not connection (old var)
				// childLogger.Println("WARN: Audio sample received but Agora rtcConnection/audio sender not ready. Dropping.")
				continue
			}
			samplePayload := new(ipcgen.MediaSamplePayload)
			samplePayload.Init(payloadTable.Bytes, payloadTable.Pos)
			if samplePayload.DataLength() == 0 {
				childLogger.Println("WARN: Received empty audio sample data.")
				continue
			}
			frameData := samplePayload.DataBytes()
			// timestampMs := samplePayload.TimestampUnixNano() / 1e6

			// Assuming PCM16 based on parent's current settings
			bytesPerSample := 2 // For PCM16
			if initAudioChannels == 0 {
				childLogger.Println("ERROR: initAudioChannels (from CLI flags) is 0, cannot calculate samplesPerChannel for audio frame.")
				continue
			}
			samplesPerChannel := len(frameData) / (int(initAudioChannels) * bytesPerSample)

			audioFrame := &agoraservice.AudioFrame{
				Type:              agoraservice.AudioFrameTypePCM16,
				SamplesPerChannel: samplesPerChannel,
				BytesPerSample:    bytesPerSample,
				Channels:          int(initAudioChannels),
				SamplesPerSec:     int(initSampleRate),
				Buffer:            frameData,
				RenderTimeMs:      int64(0),
			}
			if ret := audioSender.SendAudioPcmData(audioFrame); ret != 0 {
				childLogger.Printf("WARN: audioSender.SendAudioPcmData failed, error code: %d", ret)
			}

		case ipcgen.MessageTypeCLOSE_COMMAND:
			childLogger.Println("Received Close command. Cleaning up and exiting.")
			cleanupAgoraResources()
			sendAsyncLogResponse(ipcgen.LogLevelInfo, "Child process shutting down.")
			sendAsyncStatusResponse(ipcgen.ConnectionStatusDisconnected, "", "Closed by parent command")
			childLogger.Println("Child process terminated by close command.")
			return // Exit main loop, defer agoraservice.Release() will run.

		default:
			errMsg := fmt.Sprintf("Unknown command type received: %s", ipcgen.EnumNamesMessageType[ipcMsg.MessageType()])
			childLogger.Println(errMsg)
			sendErrorResponse(ipcgen.ConnectionStatusFailed, errMsg, "")
		}
	}
}

func setupMediaInfrastructureAndPublish(conn *agoraservice.RtcConnection) error {
	if conn == nil {
		return fmt.Errorf("RtcConnection is nil in setupMediaInfrastructureAndPublish")
	}
	localUser := conn.GetLocalUser()
	if localUser == nil {
		return fmt.Errorf("LocalUser is nil in setupMediaInfrastructureAndPublish")
	}
	if mediaFactory == nil {
		return fmt.Errorf("MediaNodeFactory is nil in setupMediaInfrastructureAndPublish")
	}

	// Create Senders
	childLogger.Println("Creating AudioPcmDataSender...")
	audioSender = mediaFactory.NewAudioPcmDataSender()
	if audioSender == nil {
		return fmt.Errorf("failed to create AudioPcmDataSender")
	}
	childLogger.Println("AudioPcmDataSender created.")

	childLogger.Println("Creating VideoFrameSender...")
	videoSender = mediaFactory.NewVideoFrameSender()
	if videoSender == nil {
		// Clean up audioSender if videoSender creation fails
		audioSender.Release()
		audioSender = nil
		return fmt.Errorf("failed to create VideoFrameSender")
	}
	childLogger.Println("VideoFrameSender created.")

	// Create Tracks
	childLogger.Println("Creating custom audio track (PCM)...")
	localAudioTrack = agoraservice.NewCustomAudioTrackPcm(audioSender)
	if localAudioTrack == nil {
		audioSender.Release()
		audioSender = nil
		videoSender.Release()
		videoSender = nil
		return fmt.Errorf("failed to create custom audio track (PCM)")
	}
	childLogger.Println("Custom audio track (PCM) created.")

	childLogger.Println("Creating custom video track (Frame)...")
	localVideoTrack = agoraservice.NewCustomVideoTrackFrame(videoSender)
	if localVideoTrack == nil {
		localAudioTrack.Release()
		localAudioTrack = nil
		audioSender.Release()
		audioSender = nil
		videoSender.Release()
		videoSender = nil
		return fmt.Errorf("failed to create custom video track (Frame)")
	}
	childLogger.Println("Custom video track (Frame) created.")

	// Configure Video Track
	videoEncoderConfig := &agoraservice.VideoEncoderConfiguration{
		CodecType:         agoraservice.VideoCodecTypeH264, // From command-line flags
		Width:             int(initWidth),                  // From command-line flags
		Height:            int(initHeight),                 // From command-line flags
		Framerate:         int(initFrameRate),              // From command-line flags
		Bitrate:           initBitrate,                     // From command-line flags (Kbps, wrapper converts to bps)
		MinBitrate:        initMinBitrate,                  // From command-line flags (Kbps, wrapper converts to bps)
		OrientationMode:   agoraservice.OrientationModeAdaptive,
		DegradePreference: agoraservice.DegradeMaintainBalanced, // Changed to MaintainBalanced as requested
	}
	childLogger.Printf("Setting video encoder configuration: %+v", videoEncoderConfig)
	if ret := localVideoTrack.SetVideoEncoderConfiguration(videoEncoderConfig); ret != 0 {
		errMsg := fmt.Sprintf("failed to set video encoder configuration, error code: %d", ret)
		cleanupLocalRtcResources(false) // Clean up created tracks/senders
		return fmt.Errorf(errMsg)
	}
	childLogger.Println("Video encoder configuration set.")

	// Enable Tracks
	childLogger.Println("Enabling local audio track...")
	localAudioTrack.SetEnabled(true)
	childLogger.Println("Enabling local video track...")
	localVideoTrack.SetEnabled(true)

	// Publish Tracks
	childLogger.Println("Publishing local audio track...")
	if ret := localUser.PublishAudio(localAudioTrack); ret != 0 {
		errMsg := fmt.Sprintf("failed to publish audio track, error code: %d", ret)
		cleanupLocalRtcResources(false) // Clean up
		return fmt.Errorf(errMsg)
	}
	childLogger.Println("Local audio track published.")

	childLogger.Println("Publishing local video track...")
	if ret := localUser.PublishVideo(localVideoTrack); ret != 0 {
		errMsg := fmt.Sprintf("failed to publish video track, error code: %d", ret)
		// Unpublish audio if video fails, then cleanup
		localUser.UnpublishAudio(localAudioTrack)
		cleanupLocalRtcResources(false) // Clean up
		return fmt.Errorf(errMsg)
	}
	childLogger.Println("Local video track published.")

	childLogger.Println("Media infrastructure setup and publishing completed successfully.")
	return nil
}

func cleanupAgoraResources() {
	childLogger.Println("Cleaning up ALL Agora resources due to CLOSE command or fatal error...")
	cleanupLocalRtcResources(true) // True to release the connection object fully

	// Global SDK release is handled by defer agoraservice.Release() in main()
	// No need to call agoraservice.Release() here directly.
	childLogger.Println("Full Agora resources cleanup attempt finished.")
}

func sendAsyncStatusResponse(status ipcgen.ConnectionStatus, message string, details string) {
	stdoutLock.Lock()
	defer stdoutLock.Unlock()

	builder := flatbuffers.NewBuilder(1024)
	msgStr := builder.CreateString(message)
	detailsStr := builder.CreateString(details)

	ipcgen.StatusResponsePayloadStart(builder)
	ipcgen.StatusResponsePayloadAddStatus(builder, status)
	ipcgen.StatusResponsePayloadAddErrorMessage(builder, msgStr)
	ipcgen.StatusResponsePayloadAddAdditionalInfo(builder, detailsStr)
	payloadOffset := ipcgen.StatusResponsePayloadEnd(builder)

	ipcgen.IPCMessageStart(builder)
	ipcgen.IPCMessageAddMessageType(builder, ipcgen.MessageTypeSTATUS_RESPONSE)
	ipcgen.IPCMessageAddPayloadType(builder, ipcgen.MessagePayloadStatus)
	ipcgen.IPCMessageAddPayload(builder, payloadOffset)
	msg := ipcgen.IPCMessageEnd(builder)
	builder.Finish(msg)

	buf := builder.FinishedBytes()
	sendFramedMessage(stdoutWriter, buf)         // stdoutWriter is *bufio.Writer
	if err := stdoutWriter.Flush(); err != nil { // Flush on *bufio.Writer
		childLogger.Printf("ERROR flushing stdout after status response: %v", err)
	}
}
func sendAsyncErrorResponse(statusForError ipcgen.ConnectionStatus, errMsgStr string, errorDetails string) {
	sendAsyncStatusResponse(statusForError, errMsgStr, errorDetails)
}
func sendAsyncLogResponse(level ipcgen.LogLevel, messageStr string) {
	stdoutLock.Lock()
	defer stdoutLock.Unlock()

	builder := flatbuffers.NewBuilder(1024)
	msgStr := builder.CreateString(messageStr)

	ipcgen.LogResponsePayloadStart(builder)
	ipcgen.LogResponsePayloadAddLevel(builder, level)
	ipcgen.LogResponsePayloadAddMessage(builder, msgStr)
	payloadOffset := ipcgen.LogResponsePayloadEnd(builder)

	ipcgen.IPCMessageStart(builder)
	ipcgen.IPCMessageAddMessageType(builder, ipcgen.MessageTypeLOG_RESPONSE)
	ipcgen.IPCMessageAddPayloadType(builder, ipcgen.MessagePayloadLog)
	ipcgen.IPCMessageAddPayload(builder, payloadOffset)
	msg := ipcgen.IPCMessageEnd(builder)
	builder.Finish(msg)

	buf := builder.FinishedBytes()
	sendFramedMessage(stdoutWriter, buf)         // stdoutWriter is *bufio.Writer
	if err := stdoutWriter.Flush(); err != nil { // Flush on *bufio.Writer
		childLogger.Printf("ERROR flushing stdout after log response: %v", err)
	}
}

// sendResponse writes to the provided writer (now global stdoutWriter via helpers)
func sendResponse(writer io.Writer, msgType ipcgen.MessageType, payloadType ipcgen.MessagePayload, payloadOffset flatbuffers.UOffsetT) {
	builder := flatbuffers.NewBuilder(256)
	ipcgen.IPCMessageStart(builder)
	ipcgen.IPCMessageAddMessageType(builder, msgType)
	if payloadOffset != 0 {
		ipcgen.IPCMessageAddPayloadType(builder, payloadType)
		ipcgen.IPCMessageAddPayload(builder, payloadOffset)
	}
	ipcMessage := ipcgen.IPCMessageEnd(builder)
	builder.Finish(ipcMessage)
	msgBytes := builder.FinishedBytes()

	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(len(msgBytes)))

	if _, err := writer.Write(lenBytes); err != nil {
		childLogger.Printf("Failed to write message length to writer: %v", err)
		return
	}
	if _, err := writer.Write(msgBytes); err != nil {
		childLogger.Printf("Failed to write message payload to writer: %v", err)
	}
}

// sendStatusResponse and sendErrorResponse now use the async helpers if called from main, or could be direct if context is clear.
// For simplicity, current direct callers in main loop will use these, which now call sendAsyncResponse -> sendResponse.
func sendStatusResponse(status ipcgen.ConnectionStatus, errMsgStr string, addInfoStr string) {
	// This writer argument is now shadowed by the global stdoutWriter used in sendAsyncStatusResponse.
	// To be correct, these should also use the async path or be refactored if direct write is from main loop only.
	sendAsyncStatusResponse(status, errMsgStr, addInfoStr)
}
func sendErrorResponse(statusForError ipcgen.ConnectionStatus, errorMessage string, errorDetails string) {
	sendAsyncStatusResponse(statusForError, errorMessage, errorDetails)
}

// sendFramedMessage writes the message length and then the message to the writer.
// Changed writer to *bufio.Writer to make Flushable contract clear for internal use.
func sendFramedMessage(writer *bufio.Writer, msg []byte) {
	lenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBytes, uint32(len(msg)))

	if _, err := writer.Write(lenBytes); err != nil {
		childLogger.Printf("Failed to write message length to writer: %v", err)
		return
	}
	if _, err := writer.Write(msg); err != nil {
		childLogger.Printf("Failed to write message payload to writer: %v", err)
	}
	// Flushing is handled by the calling async functions, as they manage the lock
	// and batching logic if any (though currently they flush immediately).
}
