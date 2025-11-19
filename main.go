package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ======================================================================================
// SECCIÓN 1: MODELOS Y ESTRUCTURAS DE DATOS
// ======================================================================================

// --- Estructuras Postman ---

type RequestDetails struct {
	Method    string
	URL       string
	Headers   string
	Body      string
	Timestamp string
	Auth      string
}

type PostmanCollection struct {
	Info struct {
		Name string `json:"name"`
	} `json:"info"`
	Items []PostmanItem `json:"item"`
}

type PostmanItem struct {
	Name    string          `json:"name"`
	Request *PostmanRequest `json:"request,omitempty"`
	Items   []PostmanItem   `json:"item,omitempty"`
}

type PostmanRequest struct {
	Method string `json:"method"`
	Url    struct {
		Raw string `json:"raw"`
	} `json:"url"`
	Header []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"header"`
	Body struct {
		Mode string `json:"mode"`
		Raw  string `json:"raw"`
	} `json:"body"`
}

// --- Estructuras Benchmark ---

type BenchmarkResult struct {
	Seq       int
	Timestamp string
	Duration  float64 // ms
	Status    int
}

type RequestConfig struct {
	URL             string
	Method          string
	Headers         string
	Body            string
	ContentType     string
	User, Secret    string
	Count           int
	Duration        int // Segundos
	ConcurrentUsers int
}

type BenchmarkStats struct {
	Avg, Min, Max, P90, P95, P99 float64
	Success, Total, ErrorRate    int
	RequestsPerSecond            float64
	TotalDuration                float64
}

// ======================================================================================
// SECCIÓN 2: WIDGET DE GRÁFICO (CORE VISUAL)
// ======================================================================================

const (
	MaxVisiblePointsNormal   = 10
	MaxVisiblePointsRealTime = 50
	FullScreenThreshold      = 15
)

type ViewMode int

const (
	ViewModeNormal ViewMode = iota
	ViewModeRealTime
	ViewModeFullScreen
)

type PointInfo struct {
	X, Y      float32
	Result    BenchmarkResult
	ExtraData string
}

type ChartWidget struct {
	widget.BaseWidget
	Data             []BenchmarkResult
	tooltip          *widget.Label
	tooltipBg        *canvas.Rectangle
	tooltipContainer *fyne.Container
	points           []PointInfo
	lastMousePos     fyne.Position
	hoverTimeout     *time.Timer
	viewMode         ViewMode
	startTime        time.Time
	lastUpdateTime   time.Time
	parent           *fyne.Container
}

func NewChartWidget() *ChartWidget {
	c := &ChartWidget{}
	c.ExtendBaseWidget(c)
	c.viewMode = ViewModeNormal
	c.startTime = time.Now()

	c.tooltip = widget.NewLabel("")
	c.tooltip.TextStyle = fyne.TextStyle{Bold: true}
	c.tooltip.Alignment = fyne.TextAlignLeading
	c.tooltip.Wrapping = fyne.TextWrapWord

	c.tooltipBg = canvas.NewRectangle(color.NRGBA{R: 50, G: 50, B: 50, A: 240})
	c.tooltipContainer = container.NewStack(c.tooltipBg, container.NewPadded(c.tooltip))
	c.tooltipContainer.Hide()

	return c
}

func (c *ChartWidget) SetData(d []BenchmarkResult) {
	c.Data = d
	c.points = nil
	c.lastUpdateTime = time.Now()

	if len(d) >= FullScreenThreshold && c.viewMode == ViewModeNormal {
		c.viewMode = ViewModeRealTime
		if len(d) >= 30 {
			c.viewMode = ViewModeFullScreen
		}
	}
	c.Refresh()
}

func (c *ChartWidget) SetViewMode(mode ViewMode) {
	c.viewMode = mode
	c.Refresh()
}

func (c *ChartWidget) GetViewMode() ViewMode {
	return c.viewMode
}

func (c *ChartWidget) CreateRenderer() fyne.WidgetRenderer {
	return &chartRenderer{chart: c}
}

func (c *ChartWidget) MouseIn(event *desktop.MouseEvent) {
	c.updateTooltip(event.Position)
}

func (c *ChartWidget) MouseMoved(event *desktop.MouseEvent) {
	c.updateTooltip(event.Position)
}

func (c *ChartWidget) MouseOut() {
	c.hideTooltip()
}

func (c *ChartWidget) updateTooltip(pos fyne.Position) {
	c.lastMousePos = pos
	if c.hoverTimeout != nil {
		c.hoverTimeout.Stop()
	}

	for _, point := range c.points {
		dx := pos.X - point.X
		dy := pos.Y - point.Y
		distance := dx*dx + dy*dy

		if distance <= 225 {
			c.showTooltip(point, pos)
			return
		}
	}

	c.hoverTimeout = time.AfterFunc(100*time.Millisecond, func() {
		fyne.Do(func() { c.hideTooltip() })
	})
}

func (c *ChartWidget) showTooltip(point PointInfo, mousePos fyne.Position) {
	if c.tooltip == nil {
		return
	}
	fyne.Do(func() {
		tooltipText := fmt.Sprintf("Seq: %d\nHora: %s\nLatencia: %.1f ms\nStatus: %d%s",
			point.Result.Seq, point.Result.Timestamp, point.Result.Duration, point.Result.Status, point.ExtraData)

		c.tooltip.SetText(tooltipText)
		tooltipX := mousePos.X + 15
		tooltipY := mousePos.Y - 10

		if tooltipX+150 > c.Size().Width {
			tooltipX = mousePos.X - 165
		}
		if tooltipY < 0 {
			tooltipY = mousePos.Y + 20
		}

		c.tooltipContainer.Resize(fyne.NewSize(150, 80))
		c.tooltipContainer.Move(fyne.NewPos(tooltipX, tooltipY))
		c.tooltipContainer.Show()
		c.Refresh()
	})
}

func (c *ChartWidget) hideTooltip() {
	fyne.Do(func() {
		if c.tooltipContainer != nil {
			c.tooltipContainer.Hide()
			c.Refresh()
		}
	})
}

// --- Chart Renderer ---

type chartRenderer struct {
	chart   *ChartWidget
	objects []fyne.CanvasObject
}

func (r *chartRenderer) MinSize() fyne.Size { return fyne.NewSize(100, 100) }

func (r *chartRenderer) Layout(size fyne.Size) {
	r.objects = r.generateChartObjects(size)
}

func (r *chartRenderer) Refresh() {
	r.Layout(r.chart.Size())
	canvas.Refresh(r.chart)
}

func (r *chartRenderer) Objects() []fyne.CanvasObject {
	if r.chart.tooltipContainer != nil {
		return append(r.objects, r.chart.tooltipContainer)
	}
	return r.objects
}

func (r *chartRenderer) Destroy() {}

func (r *chartRenderer) generateChartObjects(size fyne.Size) []fyne.CanvasObject {
	data := r.chart.Data
	maxPoints := MaxVisiblePointsNormal

	switch r.chart.viewMode {
	case ViewModeNormal:
		maxPoints = MaxVisiblePointsNormal
		if len(data) > maxPoints {
			data = data[len(data)-maxPoints:]
		}
	case ViewModeRealTime:
		maxPoints = MaxVisiblePointsRealTime
		if len(data) > maxPoints {
			step := len(data) / maxPoints
			sampledData := make([]BenchmarkResult, 0, maxPoints)
			for i := 0; i < len(data); i += step {
				sampledData = append(sampledData, data[i])
			}
			if len(sampledData) < len(data) {
				sampledData = append(sampledData, data[len(data)-1])
			}
			data = sampledData
		}
	case ViewModeFullScreen:
		maxPoints = MaxVisiblePointsRealTime * 2
		if len(data) > maxPoints {
			step := len(data) / maxPoints
			sampledData := make([]BenchmarkResult, 0, maxPoints)
			for i := 0; i < len(data); i += step {
				sampledData = append(sampledData, data[i])
			}
			data = sampledData
		}
	}

	objs := []fyne.CanvasObject{}

	paddingLeft := float32(70)
	paddingRight := float32(60)
	paddingTop := float32(40)
	paddingBottom := float32(40)

	if r.chart.viewMode == ViewModeFullScreen {
		paddingLeft = 50
		paddingRight = 50
		paddingTop = 30
		paddingBottom = 30
	}

	bg := canvas.NewRectangle(color.NRGBA{R: 30, G: 30, B: 35, A: 255})
	bg.Resize(size)
	objs = append(objs, bg)

	if len(data) < 2 {
		modeText := "normal"
		if r.chart.viewMode == ViewModeRealTime {
			modeText = "tiempo real"
		} else if r.chart.viewMode == ViewModeFullScreen {
			modeText = "pantalla completa"
		}
		txt := canvas.NewText(fmt.Sprintf("Ejecuta un test para ver el gráfico (modo: %s)", modeText), color.Gray{Y: 150})
		txt.TextSize = 14
		txt.Alignment = fyne.TextAlignCenter
		txt.Move(fyne.NewPos(size.Width/2-150, size.Height/2))
		objs = append(objs, txt)
		return objs
	}

	graphW := size.Width - paddingLeft - paddingRight
	graphH := size.Height - paddingTop - paddingBottom

	maxDur := 0.0
	minDur := 999999.0
	for _, d := range data {
		if d.Duration > maxDur {
			maxDur = d.Duration
		}
		if d.Duration < minDur {
			minDur = d.Duration
		}
	}
	if maxDur == 0 {
		maxDur = 100
	}
	maxDur *= 1.2

	xStep := graphW / float32(len(data)-1)
	yScale := graphH / float32(maxDur)

	axisColor := color.NRGBA{R: 100, G: 100, B: 100, A: 255}

	// Ejes
	xAxis := canvas.NewLine(axisColor)
	xAxis.StrokeWidth = 2
	xAxis.Position1 = fyne.NewPos(paddingLeft, size.Height-paddingBottom)
	xAxis.Position2 = fyne.NewPos(size.Width-paddingRight, size.Height-paddingBottom)
	objs = append(objs, xAxis)

	yAxis := canvas.NewLine(axisColor)
	yAxis.StrokeWidth = 2
	yAxis.Position1 = fyne.NewPos(paddingLeft, paddingTop)
	yAxis.Position2 = fyne.NewPos(paddingLeft, size.Height-paddingBottom)
	objs = append(objs, yAxis)

	drawYLabel := func(val float64, yPos float32, label string) {
		lbl := canvas.NewText(label, axisColor)
		lbl.TextSize = 9
		lbl.Alignment = fyne.TextAlignTrailing
		lbl.Move(fyne.NewPos(paddingLeft-35, yPos-6))
		grid := canvas.NewLine(color.NRGBA{R: 60, G: 60, B: 60, A: 100})
		grid.Position1 = fyne.NewPos(paddingLeft, yPos)
		grid.Position2 = fyne.NewPos(size.Width-paddingRight, yPos)
		objs = append(objs, lbl, grid)
	}

	drawYLabel(maxDur, paddingTop, fmt.Sprintf("%.0fms", maxDur))
	drawYLabel(maxDur/2, paddingTop+graphH/2, fmt.Sprintf("%.0fms", maxDur/2))
	drawYLabel(0, size.Height-paddingBottom, "0ms")

	maxRequestsPerSec := 120.0
	requestsAxisColor := color.NRGBA{R: 255, G: 193, B: 7, A: 255}
	requestsAxisX := size.Width - paddingRight
	requestsAxis := canvas.NewLine(requestsAxisColor)
	requestsAxis.StrokeWidth = 2
	requestsAxis.Position1 = fyne.NewPos(requestsAxisX, paddingTop)
	requestsAxis.Position2 = fyne.NewPos(requestsAxisX, size.Height-paddingBottom)
	objs = append(objs, requestsAxis)

	drawRequestsLabel := func(val float64, yPos float32, label string) {
		lbl := canvas.NewText(label, requestsAxisColor)
		lbl.TextSize = 9
		lbl.Alignment = fyne.TextAlignLeading
		lbl.Move(fyne.NewPos(requestsAxisX+5, yPos-6))
		objs = append(objs, lbl)
	}

	drawRequestsLabel(maxRequestsPerSec, paddingTop, "120 req/s")
	drawRequestsLabel(maxRequestsPerSec/2, paddingTop+graphH/2, "60")
	drawRequestsLabel(0, size.Height-paddingBottom, "0")

	errorAxisColor := color.NRGBA{R: 237, G: 28, B: 36, A: 255}
	errorAxisX := float32(15)
	errorAxis := canvas.NewLine(errorAxisColor)
	errorAxis.StrokeWidth = 2
	errorAxis.Position1 = fyne.NewPos(errorAxisX, paddingTop)
	errorAxis.Position2 = fyne.NewPos(errorAxisX, size.Height-paddingBottom)
	objs = append(objs, errorAxis)

	drawErrorLabel := func(val float64, yPos float32, label string) {
		lbl := canvas.NewText(label, errorAxisColor)
		lbl.TextSize = 8
		lbl.Alignment = fyne.TextAlignLeading
		lbl.Move(fyne.NewPos(errorAxisX+3, yPos-6))
		objs = append(objs, lbl)
	}

	drawErrorLabel(100, paddingTop, "100%")
	drawErrorLabel(50, paddingTop+graphH/2, "50")
	drawErrorLabel(0, size.Height-paddingBottom, "0")

	requestsScale := graphH / float32(maxRequestsPerSec)
	errorScale := graphH / 100.0

	r.chart.points = nil

	responseTimeColor := color.NRGBA{R: 0, G: 162, B: 232, A: 255}
	requestsSecColor := color.NRGBA{R: 255, G: 193, B: 7, A: 255}
	errorRateColor := color.NRGBA{R: 237, G: 28, B: 36, A: 255}

	var prevResponsePos, prevRequestsPos, prevErrorPos fyne.Position

	lineWidth := float32(2)
	pointSize := float32(4)

	if r.chart.viewMode == ViewModeFullScreen {
		lineWidth = 3
		pointSize = 5
	} else if r.chart.viewMode == ViewModeRealTime {
		lineWidth = 2.5
		pointSize = 4.5
	}

	for i, d := range data {
		x := paddingLeft + (float32(i) * xStep)
		responseY := (size.Height - paddingBottom) - (float32(d.Duration) * yScale)

		requestsPerSec := float64(i+1) / (float64(i+1) * 0.1)
		if requestsPerSec > 120 {
			requestsPerSec = 120
		}
		requestsY := (size.Height - paddingBottom) - (float32(requestsPerSec) * requestsScale)

		errorsUpToNow := float64(0)
		for j := 0; j <= i; j++ {
			if data[j].Status >= 400 || data[j].Status == 0 {
				errorsUpToNow++
			}
		}
		currentErrorRate := (errorsUpToNow / float64(i+1)) * 100
		errorY := (size.Height - paddingBottom) - (float32(currentErrorRate) * errorScale)

		responsePos := fyne.NewPos(x, responseY)
		requestsPos := fyne.NewPos(x, requestsY)
		errorPos := fyne.NewPos(x, errorY)

		if i > 0 {
			responseLine := canvas.NewLine(responseTimeColor)
			responseLine.StrokeWidth = lineWidth
			responseLine.Position1 = prevResponsePos
			responseLine.Position2 = responsePos
			objs = append(objs, responseLine)

			requestsLine := canvas.NewLine(requestsSecColor)
			requestsLine.StrokeWidth = lineWidth
			requestsLine.Position1 = prevRequestsPos
			requestsLine.Position2 = requestsPos
			objs = append(objs, requestsLine)

			errorLine := canvas.NewLine(errorRateColor)
			errorLine.StrokeWidth = lineWidth
			errorLine.Position1 = prevErrorPos
			errorLine.Position2 = errorPos
			objs = append(objs, errorLine)
		}

		if r.chart.viewMode != ViewModeFullScreen {
			responseDot := canvas.NewCircle(responseTimeColor)
			responseDot.Resize(fyne.NewSize(pointSize, pointSize))
			responseDot.Move(fyne.NewPos(x-pointSize/2, responseY-pointSize/2))
			objs = append(objs, responseDot)

			errorDot := canvas.NewCircle(errorRateColor)
			errorDot.Resize(fyne.NewSize(pointSize+2, pointSize+2))
			errorDot.Move(fyne.NewPos(x-(pointSize+2)/2, errorY-(pointSize+2)/2))
			objs = append(objs, errorDot)

			if currentErrorRate > 0 {
				errorLabel := canvas.NewText(fmt.Sprintf("%.1f%%", currentErrorRate), errorRateColor)
				errorLabel.TextSize = 8
				errorLabel.Alignment = fyne.TextAlignCenter
				errorLabel.Move(fyne.NewPos(x-12, errorY-18))
				objs = append(objs, errorLabel)
			}
		}

		lblText := fmt.Sprintf("#%d", d.Seq)
		showLabel := false

		switch r.chart.viewMode {
		case ViewModeNormal:
			if len(data) <= 5 {
				lblText = d.Timestamp
				showLabel = true
			} else {
				showLabel = i%2 == 0 || i == len(data)-1
			}
		case ViewModeRealTime:
			showLabel = i%5 == 0 || i == len(data)-1
		case ViewModeFullScreen:
			showLabel = i%10 == 0 || i == len(data)-1
			lblText = d.Timestamp
		}

		if showLabel {
			xLbl := canvas.NewText(lblText, axisColor)
			xLbl.TextSize = 9
			xLbl.Alignment = fyne.TextAlignCenter
			xLbl.Move(fyne.NewPos(x-15, size.Height-paddingBottom+5))
			objs = append(objs, xLbl)
		}

		if r.chart.viewMode == ViewModeNormal {
			win := fyne.CurrentApp().Driver().AllWindows()[0]

			responseInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Avg Response\n\nSeq: %d\nHora: %s\nLatencia: %.2f ms\nStatus: %d\nRequests/sec: %.1f\nError rate: %.1f%%\nTiempo transcurrido: %.1fs",
				d.Seq, d.Timestamp, d.Duration, d.Status, requestsPerSec, currentErrorRate, float64(i+1)*0.1)
			responseBtn := widget.NewButton("", nil)
			responseBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Avg Response", responseInfoTxt, win) }
			responseBtn.Resize(fyne.NewSize(15, 15))
			responseBtn.Move(fyne.NewPos(x-7, responseY-7))
			responseBtn.Importance = widget.LowImportance
			objs = append(objs, responseBtn)

			requestsInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Requests/Second\n\nSeq: %d\nHora: %s\nRequests/sec: %.1f\nLatencia: %.2f ms\nStatus: %d\nError rate: %.1f%%",
				d.Seq, d.Timestamp, requestsPerSec, d.Duration, d.Status, currentErrorRate)
			requestsBtn := widget.NewButton("", nil)
			requestsBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Requests/Second", requestsInfoTxt, win) }
			requestsBtn.Resize(fyne.NewSize(15, 15))
			requestsBtn.Move(fyne.NewPos(x-7, requestsY-7))
			requestsBtn.Importance = widget.LowImportance
			objs = append(objs, requestsBtn)

			errorInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Error Rate\n\nSeq: %d\nHora: %s\nError rate: %.1f%%\nErrores acumulados: %.0f de %d\nLatencia: %.2f ms\nStatus: %d",
				d.Seq, d.Timestamp, currentErrorRate, errorsUpToNow, i+1, d.Duration, d.Status)
			errorBtn := widget.NewButton("", nil)
			errorBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Error Rate", errorInfoTxt, win) }
			errorBtn.Resize(fyne.NewSize(15, 15))
			errorBtn.Move(fyne.NewPos(x-7, errorY-7))
			errorBtn.Importance = widget.LowImportance
			objs = append(objs, errorBtn)
		}

		extraInfo := fmt.Sprintf("\nRequests/sec: %.1f\nError rate: %.1f%%", requestsPerSec, currentErrorRate)
		r.chart.points = append(r.chart.points, PointInfo{X: x, Y: responseY, Result: d, ExtraData: extraInfo})

		requestsInfo := fmt.Sprintf("\nRequests/sec: %.1f\nLatencia: %.2f ms\nError rate: %.1f%%", requestsPerSec, d.Duration, currentErrorRate)
		r.chart.points = append(r.chart.points, PointInfo{X: x, Y: requestsY, Result: d, ExtraData: requestsInfo})

		errorInfo := fmt.Sprintf("\nError rate: %.1f%%\nErrores: %.0f de %d\nRequests/sec: %.1f\nLatencia: %.2f ms", currentErrorRate, errorsUpToNow, i+1, requestsPerSec, d.Duration)
		r.chart.points = append(r.chart.points, PointInfo{X: x, Y: errorY, Result: d, ExtraData: errorInfo})

		prevResponsePos = responsePos
		prevRequestsPos = requestsPos
		prevErrorPos = errorPos
	}

	legendY := paddingTop + 10
	legendItems := []struct {
		color color.NRGBA
		text  string
	}{
		{responseTimeColor, "Avg. response"},
		{requestsSecColor, "Requests/second"},
		{errorRateColor, "Error rate"},
	}

	for i, item := range legendItems {
		legendX := paddingLeft + float32(i*120)
		legendLine := canvas.NewLine(item.color)
		legendLine.StrokeWidth = 2
		legendLine.Position1 = fyne.NewPos(legendX, legendY)
		legendLine.Position2 = fyne.NewPos(legendX+15, legendY)
		objs = append(objs, legendLine)

		legendText := canvas.NewText(item.text, color.White)
		legendText.TextSize = 9
		legendText.Move(fyne.NewPos(legendX+20, legendY-6))
		objs = append(objs, legendText)
	}

	return objs
}

// ======================================================================================
// SECCIÓN 3: LÓGICA DE NEGOCIO Y HELPERS
// ======================================================================================

func generateHMACSignature(secretKey, message string) string {
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
}

func parseCurlCommand(curl string, urlEntry *widget.Entry, methodSelect *widget.Select, headersEntry *widget.Entry, bodyEntry *widget.Entry) {
	curl = strings.TrimSpace(curl)
	curl = strings.ReplaceAll(curl, "\r\n", " ")
	curl = strings.ReplaceAll(curl, "\n", " ")
	curl = strings.ReplaceAll(curl, "\\", "")

	var foundURL bool

	if strings.Contains(curl, "curl") {
		for _, quote := range []string{"'", "\"", ""} {
			if foundURL {
				break
			}
			if quote != "" {
				pattern := quote + "http"
				if idx := strings.Index(curl, pattern); idx != -1 {
					startPos := idx + len(quote)
					endIdx := strings.Index(curl[startPos:], quote)
					if endIdx > 0 {
						url := curl[startPos : startPos+endIdx]
						urlEntry.SetText(url)
						foundURL = true
					}
				}
			} else {
				parts := strings.Fields(curl)
				for i, part := range parts {
					if strings.HasPrefix(part, "http") {
						urlEntry.SetText(strings.Trim(part, `"'`))
						foundURL = true
						break
					}
					if part == "curl" && i+1 < len(parts) {
						nextPart := parts[i+1]
						if strings.HasPrefix(nextPart, "http") {
							urlEntry.SetText(strings.Trim(nextPart, `"'`))
							foundURL = true
							break
						}
					}
				}
			}
		}
	}

	var methodFound bool
	if idx := strings.Index(curl, "-X "); idx != -1 {
		fields := strings.Fields(curl[idx:])
		if len(fields) > 1 {
			methodSelect.SetSelected(strings.ToUpper(strings.Trim(fields[1], `"'`)))
			methodFound = true
		}
	}

	if !methodFound {
		if idx := strings.Index(curl, "--request "); idx != -1 {
			fields := strings.Fields(curl[idx:])
			if len(fields) > 1 {
				methodSelect.SetSelected(strings.ToUpper(strings.Trim(fields[1], `"'`)))
				methodFound = true
			}
		}
	}

	if !methodFound {
		if strings.Contains(curl, "--data") || strings.Contains(curl, "-d ") {
			methodSelect.SetSelected("POST")
		}
	}

	headers := []string{}
	searchPatterns := []string{"-H '", `-H "`, "--header '", `--header "`}

	for _, pattern := range searchPatterns {
		startIdx := 0
		for {
			idx := strings.Index(curl[startIdx:], pattern)
			if idx == -1 {
				break
			}

			actualIdx := startIdx + idx
			startPos := actualIdx + len(pattern)
			var endDelimiter string
			if strings.HasSuffix(pattern, "'") {
				endDelimiter = "'"
			} else {
				endDelimiter = `"`
			}

			endIdx := strings.Index(curl[startPos:], endDelimiter)
			if endIdx > 0 {
				headerContent := curl[startPos : startPos+endIdx]
				if headerContent != "" {
					headers = append(headers, headerContent)
				}
			}

			startIdx = actualIdx + len(pattern) + 1
			if startIdx >= len(curl) {
				break
			}
		}
	}

	if len(headers) > 0 {
		headersEntry.SetText(strings.Join(headers, "\n"))
	}

	if idx := strings.Index(curl, "--data-raw '"); idx != -1 {
		start := idx + len("--data-raw '")
		end := strings.Index(curl[start:], "'")
		if end > 0 {
			bodyEntry.SetText(curl[start : start+end])
			return
		}
	}
	if idx := strings.Index(curl, `--data-raw "`); idx != -1 {
		start := idx + len(`--data-raw "`)
		end := strings.Index(curl[start:], `"`)
		if end > 0 {
			bodyEntry.SetText(curl[start : start+end])
			return
		}
	}
	if idx := strings.Index(curl, "-d '"); idx != -1 {
		start := idx + len("-d '")
		end := strings.Index(curl[start:], "'")
		if end > 0 {
			bodyEntry.SetText(curl[start : start+end])
			return
		}
	}
	if idx := strings.Index(curl, `-d "`); idx != -1 {
		start := idx + len(`-d "`)
		end := strings.Index(curl[start:], `"`)
		if end > 0 {
			bodyEntry.SetText(curl[start : start+end])
		}
	}
}

func runLoadTest(cfg RequestConfig, progress func(float64), cancelChan <-chan bool, realtimeUpdate func([]BenchmarkResult, BenchmarkStats)) ([]BenchmarkResult, BenchmarkStats) {
	results := make([]BenchmarkResult, 0)
	resultsMutex := sync.Mutex{}
	successCount := 0
	var totalDuration float64
	minDur := 999999.0
	maxDur := 0.0
	startTime := time.Now()
	var endTime time.Time

	useDuration := cfg.Duration > 0
	if useDuration {
		endTime = startTime.Add(time.Duration(cfg.Duration) * time.Second)
	}

	var wg sync.WaitGroup

	executeUser := func(userID int) {
		defer wg.Done()
		client := &http.Client{Timeout: 10 * time.Second}

		for {
			select {
			case <-cancelChan:
				return
			default:
			}

			if useDuration {
				if time.Now().After(endTime) {
					break
				}
			} else {
				resultsMutex.Lock()
				currentTotal := len(results)
				resultsMutex.Unlock()
				if currentTotal >= cfg.Count {
					break
				}
			}

			if useDuration && time.Now().Add(10*time.Second).After(endTime) {
				break
			}

			var bodyReader io.Reader
			if cfg.Body != "" {
				bodyReader = strings.NewReader(cfg.Body)
			}

			req, err := http.NewRequest(cfg.Method, cfg.URL, bodyReader)
			if err == nil {
				timestamp := time.Now().Format(time.RFC3339)
				req.Header.Set("X-Timestamp", timestamp)
				if cfg.ContentType != "" {
					req.Header.Set("Content-Type", cfg.ContentType)
				}
				if cfg.Headers != "" {
					for _, line := range strings.Split(cfg.Headers, "\n") {
						parts := strings.SplitN(line, ":", 2)
						if len(parts) == 2 {
							req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
						}
					}
				}
				if cfg.User != "" && cfg.Secret != "" {
					sig := generateHMACSignature(cfg.Secret, timestamp)
					req.Header.Set("Authorization", fmt.Sprintf("HMAC %s:%s", cfg.User, sig))
				}

				start := time.Now()
				resp, err := client.Do(req)
				duration := float64(time.Since(start).Milliseconds())

				status := 0
				if err == nil {
					status = resp.StatusCode
					resp.Body.Close()
					if status >= 200 && status < 400 {
						resultsMutex.Lock()
						successCount++
						resultsMutex.Unlock()
					}
				}

				resultsMutex.Lock()
				totalDuration += duration
				if duration < minDur {
					minDur = duration
				}
				if duration > maxDur {
					maxDur = duration
				}
				results = append(results, BenchmarkResult{
					Seq:       len(results) + 1,
					Timestamp: start.Format("15:04:05"),
					Duration:  duration,
					Status:    status,
				})
				currentTotal := len(results)
				resultsCopy := make([]BenchmarkResult, len(results))
				copy(resultsCopy, results)
				resultsMutex.Unlock()

				if progress != nil {
					var progressValue float64
					if useDuration {
						elapsed := time.Since(startTime).Seconds()
						progressValue = elapsed / float64(cfg.Duration)
					} else {
						progressValue = float64(currentTotal) / float64(cfg.Count)
					}
					progress(progressValue)
				}

				if realtimeUpdate != nil && currentTotal%5 == 0 {
					partialStats := BenchmarkStats{
						Total:         currentTotal,
						Success:       successCount,
						Min:           minDur,
						Max:           maxDur,
						TotalDuration: totalDuration,
					}
					if partialStats.Total > 0 {
						partialStats.Avg = totalDuration / float64(partialStats.Total)
						partialStats.ErrorRate = ((partialStats.Total - partialStats.Success) * 100) / partialStats.Total
						actualDuration := time.Since(startTime).Seconds()
						partialStats.RequestsPerSecond = float64(partialStats.Total) / actualDuration
					}
					realtimeUpdate(resultsCopy, partialStats)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	users := cfg.ConcurrentUsers
	if users < 1 {
		users = 1
	}
	for i := 0; i < users; i++ {
		wg.Add(1)
		go executeUser(i)
	}
	wg.Wait()

	resultsMutex.Lock()
	durations := make([]float64, len(results))
	for i, r := range results {
		durations[i] = r.Duration
	}
	resultsMutex.Unlock()

	for i := 0; i < len(durations); i++ {
		for j := i + 1; j < len(durations); j++ {
			if durations[i] > durations[j] {
				durations[i], durations[j] = durations[j], durations[i]
			}
		}
	}

	stats := BenchmarkStats{
		Total:         len(results),
		Success:       successCount,
		Min:           minDur,
		Max:           maxDur,
		TotalDuration: totalDuration,
	}

	if stats.Total > 0 {
		stats.Avg = totalDuration / float64(stats.Total)
		stats.ErrorRate = ((stats.Total - stats.Success) * 100) / stats.Total
		actualDuration := time.Since(startTime).Seconds()
		stats.RequestsPerSecond = float64(stats.Total) / actualDuration

		if len(durations) > 0 {
			p90Index := int(0.9 * float64(len(durations)))
			p95Index := int(0.95 * float64(len(durations)))
			p99Index := int(0.99 * float64(len(durations)))

			if p90Index >= len(durations) {
				p90Index = len(durations) - 1
			}
			if p95Index >= len(durations) {
				p95Index = len(durations) - 1
			}
			if p99Index >= len(durations) {
				p99Index = len(durations) - 1
			}
			stats.P90 = durations[p90Index]
			stats.P95 = durations[p95Index]
			stats.P99 = durations[p99Index]
		}
	} else {
		stats.Min = 0
	}

	return results, stats
}

// --- Custom Layout Helper ---

type compactPaddingLayout struct {
	padding float32
}

func (c *compactPaddingLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	padding := c.padding
	if padding == 0 {
		padding = 2
	}
	pos := fyne.NewPos(padding, padding)
	for _, o := range objects {
		size := fyne.NewSize(size.Width-2*padding, size.Height-2*padding)
		o.Resize(size)
		o.Move(pos)
	}
}

func (c *compactPaddingLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	padding := c.padding
	if padding == 0 {
		padding = 2
	}
	minSize := fyne.NewSize(0, 0)
	for _, o := range objects {
		childSize := o.MinSize()
		if childSize.Width > minSize.Width {
			minSize.Width = childSize.Width
		}
		if childSize.Height > minSize.Height {
			minSize.Height = childSize.Height
		}
	}
	return fyne.NewSize(minSize.Width+2*padding, minSize.Height+2*padding)
}

func newBoldLabel(text string, align fyne.TextAlign) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = fyne.TextStyle{Bold: true}
	l.Alignment = align
	return l
}

// ======================================================================================
// SECCIÓN 4: ESTRUCTURA PRINCIPAL DE LA APP
// ======================================================================================

type BenchmarkApp struct {
	App    fyne.App
	Window fyne.Window

	// Input Widgets
	UrlEntry       *widget.Entry
	UserEntry      *widget.Entry
	SecretEntry    *widget.Entry
	MethodSelect   *widget.Select
	HeadersEntry   *widget.Entry
	BodyEntry      *widget.Entry
	TestModeSelect *widget.Select
	CountEntry     *widget.Entry
	DurationEntry  *widget.Entry
	TimeUnitSelect *widget.Select
	UsersEntry     *widget.Entry
	ValueContainer *fyne.Container

	// Actions
	RunBtn           *widget.Button
	FormatBtn        *widget.Button
	ConsoleToggleBtn *widget.Button
	ImportBtn        *widget.Button
	CurlBtn          *widget.Button

	// Output/Vis Widgets
	ConsoleEntry     *widget.Entry
	ConsoleContainer *fyne.Container
	PostmanTree      *widget.Tree
	ChartWidget      *ChartWidget
	ProgressBar      *widget.ProgressBar
	ResponseViewer   *widget.Entry
	StatsContainer   *fyne.Container
	RightContentArea *fyne.Container
	ChartBg          *canvas.Rectangle

	// State
	AvgBind        binding.String
	MinBind        binding.String
	MaxBind        binding.String
	SuccessBind    binding.String
	TreeData       map[string]PostmanItem
	TreeRoots      []string
	IsRunning      bool
	CancelChan     chan bool
	ConsoleVisible bool
	IsFullScreen   bool
	OriginalCont   fyne.CanvasObject
}

func NewBenchmarkApp() *BenchmarkApp {
	a := &BenchmarkApp{}
	a.App = app.NewWithID("com.francisco.benchmarkpro")
	a.Window = a.App.NewWindow("Benchmark Pro - Postman Integrado")
	a.Window.Resize(fyne.NewSize(1000, 700))
	return a
}

func (ui *BenchmarkApp) setupUI() {
	// Inputs Básicos
	ui.UrlEntry = widget.NewEntry()
	ui.UrlEntry.SetText("https://google.com")
	ui.UrlEntry.SetPlaceHolder("https://api...")

	ui.UserEntry = widget.NewEntry()
	ui.UserEntry.SetPlaceHolder("User ID")
	ui.SecretEntry = widget.NewPasswordEntry()
	ui.SecretEntry.SetPlaceHolder("HMAC Secret")

	ui.MethodSelect = widget.NewSelect([]string{"GET", "POST", "PUT", "DELETE"}, nil)
	ui.MethodSelect.Selected = "GET"

	ui.HeadersEntry = widget.NewMultiLineEntry()
	ui.HeadersEntry.SetPlaceHolder("Content-Type: application/json\nAuthorization: Bearer token")
	ui.HeadersEntry.SetMinRowsVisible(4)

	ui.BodyEntry = widget.NewMultiLineEntry()
	ui.BodyEntry.SetPlaceHolder(`{\n  "key": "value",\n  "nested": {\n    "data": "example"\n  }\n}`)
	ui.BodyEntry.SetMinRowsVisible(15)
	ui.BodyEntry.Wrapping = fyne.TextWrapWord

	// Configuración de Test
	ui.TestModeSelect = widget.NewSelect([]string{"Por Cantidad", "Por Tiempo"}, nil)
	ui.TestModeSelect.SetSelected("Por Cantidad")

	ui.CountEntry = widget.NewEntry()
	ui.CountEntry.SetText("10")
	ui.CountEntry.SetPlaceHolder("Ej: 10, 50, 100")

	ui.DurationEntry = widget.NewEntry()
	ui.DurationEntry.SetText("1")
	ui.DurationEntry.SetPlaceHolder("Ej: 1, 5, 10")
	ui.DurationEntry.Hide()

	ui.TimeUnitSelect = widget.NewSelect([]string{"Segundos", "Minutos", "Horas"}, nil)
	ui.TimeUnitSelect.SetSelected("Minutos")
	ui.TimeUnitSelect.Hide()

	ui.UsersEntry = widget.NewEntry()
	ui.UsersEntry.SetText("1")
	ui.UsersEntry.SetPlaceHolder("Usuarios concurrentes")

	durationWithUnit := container.NewHBox(ui.DurationEntry, ui.TimeUnitSelect)
	ui.ValueContainer = container.NewStack(ui.CountEntry, durationWithUnit)

	// Consola y Salidas
	ui.ConsoleEntry = widget.NewMultiLineEntry()
	ui.ConsoleEntry.Wrapping = fyne.TextWrapWord
	ui.ConsoleEntry.SetMinRowsVisible(10)
	consoleBg := canvas.NewRectangle(color.NRGBA{R: 20, G: 20, B: 25, A: 255})
	consoleDisplay := container.NewStack(consoleBg, container.NewPadded(ui.ConsoleEntry))
	consoleScrollContainer := container.NewVScroll(consoleDisplay)
	consoleScrollContainer.SetMinSize(fyne.NewSize(0, 250))

	ui.ConsoleContainer = container.NewVBox(
		widget.NewSeparator(),
		newBoldLabel("Detalles de la Request Enviada", fyne.TextAlignLeading),
		consoleScrollContainer,
	)
	ui.ConsoleContainer.Hide()

	ui.ChartWidget = NewChartWidget()
	ui.ProgressBar = widget.NewProgressBar()
	ui.ProgressBar.Hide()

	ui.ResponseViewer = widget.NewMultiLineEntry()
	ui.ResponseViewer.SetPlaceHolder("Respuesta del servidor aparecerá aquí...")
	ui.ResponseViewer.Wrapping = fyne.TextWrapWord

	ui.AvgBind = binding.NewString()
	ui.MinBind = binding.NewString()
	ui.MaxBind = binding.NewString()
	ui.SuccessBind = binding.NewString()
	ui.AvgBind.Set("Promedio: -")
	ui.MinBind.Set("Mínimo: -")
	ui.MaxBind.Set("Máximo: -")
	ui.SuccessBind.Set("Éxito: -")

	ui.StatsContainer = container.NewGridWithColumns(10)
	ui.StatsContainer.Objects = createStatsWidgets(ui.AvgBind, ui.MinBind, ui.MaxBind, ui.SuccessBind, 0)

	ui.ChartBg = canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255})
	ui.RightContentArea = container.NewStack(ui.ChartBg, ui.ChartWidget)
}

func (ui *BenchmarkApp) setupHandlers() {
	// Toggle Modo Test
	ui.TestModeSelect.OnChanged = func(mode string) {
		if mode == "Por Tiempo" {
			ui.CountEntry.Hide()
			ui.DurationEntry.Show()
			ui.TimeUnitSelect.Show()
			ui.ValueContainer.Refresh()
		} else {
			ui.DurationEntry.Hide()
			ui.TimeUnitSelect.Hide()
			ui.CountEntry.Show()
			ui.ValueContainer.Refresh()
		}
	}

	// Toggle Consola
	ui.ConsoleToggleBtn = widget.NewButtonWithIcon("Mostrar Consola Request", theme.InfoIcon(), nil)
	ui.ConsoleToggleBtn.OnTapped = func() {
		if ui.ConsoleVisible {
			ui.ConsoleContainer.Hide()
			ui.ConsoleToggleBtn.SetText("Mostrar Consola Request")
			ui.ConsoleToggleBtn.SetIcon(theme.InfoIcon())
			ui.ConsoleVisible = false
		} else {
			ui.ConsoleContainer.Show()
			ui.ConsoleToggleBtn.SetText("Ocultar Consola Request")
			ui.ConsoleToggleBtn.SetIcon(theme.ContentClearIcon())
			ui.ConsoleVisible = true
		}
	}

	// Formatear Body
	ui.FormatBtn = widget.NewButtonWithIcon("Formatear Body", theme.DocumentIcon(), func() {
		body := strings.TrimSpace(ui.BodyEntry.Text)
		if body == "" {
			return
		}
		var jsonData interface{}
		if err := json.Unmarshal([]byte(body), &jsonData); err == nil {
			formatted, err := json.MarshalIndent(jsonData, "", "  ")
			if err == nil {
				ui.BodyEntry.SetText(string(formatted))
				return
			}
		}
		if strings.HasPrefix(body, "<") {
			formatted := strings.ReplaceAll(body, "><", ">\n<")
			ui.BodyEntry.SetText(formatted)
			return
		}
		dialog.ShowInformation("Formateo", "No se pudo formatear. Asegúrate de que sea JSON o XML válido.", ui.Window)
	})

	// Ejecutar
	ui.RunBtn = widget.NewButtonWithIcon("Ejecutar Request", theme.MediaPlayIcon(), nil)
	ui.RunBtn.OnTapped = ui.handleRun

	// Importadores
	ui.ImportBtn = widget.NewButtonWithIcon("Cargar JSON Postman", theme.FolderOpenIcon(), func() {
		fd := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			defer reader.Close()
			byteValue, _ := io.ReadAll(reader)
			var collection PostmanCollection
			err = json.Unmarshal(byteValue, &collection)
			if err != nil {
				dialog.ShowError(fmt.Errorf("Error al parsear Postman JSON: %w", err), ui.Window)
				return
			}
			ui.TreeData = make(map[string]PostmanItem)
			ui.TreeRoots = []string{}
			ui.processPostmanItems(collection.Items, "")
			ui.PostmanTree.Refresh()
		}, ui.Window)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".json"}))
		fd.Show()
	})

	ui.CurlBtn = widget.NewButtonWithIcon("Pegar cURL", theme.ContentPasteIcon(), func() {
		curlEntry := widget.NewMultiLineEntry()
		curlEntry.SetPlaceHolder("Pega aquí tu comando cURL completo...")
		curlEntry.SetMinRowsVisible(10)
		formDialog := dialog.NewForm("Importar desde cURL", "Importar", "Cancelar",
			[]*widget.FormItem{widget.NewFormItem("Comando cURL:", curlEntry)},
			func(ok bool) {
				if !ok || curlEntry.Text == "" {
					return
				}
				parseCurlCommand(curlEntry.Text, ui.UrlEntry, ui.MethodSelect, ui.HeadersEntry, ui.BodyEntry)
			}, ui.Window)
		formDialog.Resize(fyne.NewSize(800, 400))
		formDialog.Show()
	})
}

func (ui *BenchmarkApp) processPostmanItems(items []PostmanItem, parentID string) {
	for i, item := range items {
		id := item.Name + strconv.Itoa(i)
		if parentID != "" {
			id = parentID + "/" + id
		}
		ui.TreeData[id] = item
		if parentID == "" {
			ui.TreeRoots = append(ui.TreeRoots, id)
		}
		if len(item.Items) > 0 {
			ui.processPostmanItems(item.Items, id)
		}
	}
}

func (ui *BenchmarkApp) updateConsole(details RequestDetails) {
	consoleText := fmt.Sprintf(`=== REQUEST ENVIADA ===

Método: %s
URL: %s
Timestamp: %s

--- HEADERS ---
%s

--- BODY ---
%s

--- AUTH ---
%s`,
		details.Method, details.URL, details.Timestamp, details.Headers, details.Body, details.Auth)
	ui.ConsoleEntry.SetText(consoleText)
}

func (ui *BenchmarkApp) handleRun() {
	if ui.IsRunning {
		if ui.CancelChan != nil {
			close(ui.CancelChan)
			ui.RunBtn.SetText("Cancelando...")
			ui.RunBtn.Disable()
		}
		return
	}

	if ui.UrlEntry.Text == "" {
		dialog.ShowError(fmt.Errorf("por favor ingresa una URL"), ui.Window)
		return
	}

	ui.ChartWidget.SetData([]BenchmarkResult{})
	ui.ResponseViewer.SetText("")
	ui.AvgBind.Set("Promedio: -")
	ui.MinBind.Set("Mínimo: -")
	ui.MaxBind.Set("Máximo: -")
	ui.SuccessBind.Set("Éxito: -")
	ui.StatsContainer.Objects = createStatsWidgets(ui.AvgBind, ui.MinBind, ui.MaxBind, ui.SuccessBind, 0)
	ui.StatsContainer.Refresh()
	ui.RightContentArea.Objects = []fyne.CanvasObject{ui.ChartBg, ui.ChartWidget}
	ui.RightContentArea.Refresh()

	ui.RunBtn.SetText("Cancelar")
	ui.RunBtn.SetIcon(theme.CancelIcon())
	ui.IsRunning = true
	ui.CancelChan = make(chan bool)
	ui.ProgressBar.Show()
	ui.ProgressBar.SetValue(0)

	count := 1
	duration := 0
	users := 1

	if ui.TestModeSelect.Selected == "Por Tiempo" {
		var durationValue int
		fmt.Sscanf(ui.DurationEntry.Text, "%d", &durationValue)
		if durationValue <= 0 {
			dialog.ShowError(fmt.Errorf("ingresa una duración válida"), ui.Window)
			ui.resetRunState()
			return
		}
		switch ui.TimeUnitSelect.Selected {
		case "Minutos":
			duration = durationValue * 60
		case "Horas":
			duration = durationValue * 3600
		default:
			duration = durationValue
		}
	} else {
		fmt.Sscanf(ui.CountEntry.Text, "%d", &count)
		if count <= 0 {
			dialog.ShowError(fmt.Errorf("ingresa una cantidad válida de peticiones"), ui.Window)
			ui.resetRunState()
			return
		}
	}

	fmt.Sscanf(ui.UsersEntry.Text, "%d", &users)
	if users <= 0 {
		users = 1
	}

	cfg := RequestConfig{
		URL: ui.UrlEntry.Text, Method: ui.MethodSelect.Selected,
		Headers: ui.HeadersEntry.Text, Body: ui.BodyEntry.Text,
		Count: count, Duration: duration, ConcurrentUsers: users,
		User: ui.UserEntry.Text, Secret: ui.SecretEntry.Text,
	}

	resultChan := make(chan []BenchmarkResult)
	statsChan := make(chan BenchmarkStats)
	progressChan := make(chan float64)

	go func() {
		for progress := range progressChan {
			fyne.Do(func() { ui.ProgressBar.SetValue(progress) })
		}
	}()

	go func() {
		defer close(progressChan)
		defer close(resultChan)
		defer close(statsChan)

		if count == 1 && duration == 0 {
			ui.runSingleRequest(cfg, resultChan, statsChan)
		} else {
			ui.runBenchmark(cfg, resultChan, statsChan, progressChan)
		}
	}()

	go ui.handleResults(resultChan, statsChan, count, duration, users)
}

func (ui *BenchmarkApp) runSingleRequest(cfg RequestConfig, resultChan chan<- []BenchmarkResult, statsChan chan<- BenchmarkStats) {
	client := &http.Client{Timeout: 10 * time.Second}
	var bodyReader io.Reader
	if cfg.Body != "" {
		bodyReader = strings.NewReader(cfg.Body)
	}

	req, err := http.NewRequest(cfg.Method, cfg.URL, bodyReader)
	if err == nil {
		timestamp := time.Now().Format(time.RFC3339)
		req.Header.Set("X-Timestamp", timestamp)
		if cfg.ContentType != "" {
			req.Header.Set("Content-Type", cfg.ContentType)
		}
		if cfg.Headers != "" {
			for _, line := range strings.Split(cfg.Headers, "\n") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					req.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
				}
			}
		}
		var authInfo string
		if cfg.User != "" && cfg.Secret != "" {
			sig := generateHMACSignature(cfg.Secret, timestamp)
			req.Header.Set("Authorization", fmt.Sprintf("HMAC %s:%s", cfg.User, sig))
			authInfo = fmt.Sprintf("HMAC - User: %s, Signature: %s", cfg.User, sig)
		} else {
			authInfo = "Sin autenticación"
		}

		var actualHeaders strings.Builder
		for name, values := range req.Header {
			for _, value := range values {
				actualHeaders.WriteString(fmt.Sprintf("%s: %s\n", name, value))
			}
		}

		fyne.Do(func() {
			ui.updateConsole(RequestDetails{
				Method:    req.Method,
				URL:       req.URL.String(),
				Headers:   actualHeaders.String(),
				Body:      cfg.Body,
				Timestamp: timestamp,
				Auth:      authInfo,
			})
		})

		start := time.Now()
		resp, err := client.Do(req)
		duration := float64(time.Since(start).Milliseconds())
		status := 0
		var responseBody string
		if err == nil {
			status = resp.StatusCode
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			responseBody = string(bodyBytes)
		} else {
			responseBody = fmt.Sprintf("Error: %v", err)
		}

		result := BenchmarkResult{Seq: 1, Timestamp: start.Format("15:04:05"), Duration: duration, Status: status}
		responseChan := make(chan string, 1)
		responseChan <- responseBody
		close(responseChan)

		fyne.Do(func() {
			responseText := fmt.Sprintf("STATUS: %d\nDURATION: %.2f ms\nTIMESTAMP: %s\n\n--- RESPONSE BODY ---\n\n%s",
				status, duration, start.Format("15:04:05"), <-responseChan)
			ui.ResponseViewer.SetText(responseText)
			ui.RightContentArea.Objects = []fyne.CanvasObject{
				canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255}),
				container.NewScroll(ui.ResponseViewer),
			}
			ui.RightContentArea.Refresh()
		})

		resultChan <- []BenchmarkResult{result}
		statsChan <- BenchmarkStats{
			Avg: duration, Min: duration, Max: duration,
			P90: duration, P95: duration, P99: duration,
			Success: 1, Total: 1, ErrorRate: 0,
			RequestsPerSecond: 1.0 / (duration / 1000.0), TotalDuration: duration,
		}
	}
}

func (ui *BenchmarkApp) runBenchmark(cfg RequestConfig, resultChan chan<- []BenchmarkResult, statsChan chan<- BenchmarkStats, progressChan chan<- float64) {
	// Simulación para consola
	var bodyReader io.Reader
	if cfg.Body != "" {
		bodyReader = strings.NewReader(cfg.Body)
	}
	sampleReq, _ := http.NewRequest(cfg.Method, cfg.URL, bodyReader)
	if sampleReq != nil {
		timestamp := time.Now().Format(time.RFC3339)
		sampleReq.Header.Set("X-Timestamp", timestamp)
		if cfg.ContentType != "" {
			sampleReq.Header.Set("Content-Type", cfg.ContentType)
		}
		if cfg.Headers != "" {
			for _, line := range strings.Split(cfg.Headers, "\n") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					sampleReq.Header.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
				}
			}
		}
		var authInfo string
		if cfg.User != "" && cfg.Secret != "" {
			sig := generateHMACSignature(cfg.Secret, timestamp)
			sampleReq.Header.Set("Authorization", fmt.Sprintf("HMAC %s:%s", cfg.User, sig))
			authInfo = fmt.Sprintf("HMAC - User: %s, Signature: %s", cfg.User, sig)
		} else {
			authInfo = "Sin autenticación"
		}

		var actualHeaders strings.Builder
		for name, values := range sampleReq.Header {
			for _, value := range values {
				actualHeaders.WriteString(fmt.Sprintf("%s: %s\n", name, value))
			}
		}
		fyne.Do(func() {
			ui.updateConsole(RequestDetails{
				Method:    sampleReq.Method,
				URL:       sampleReq.URL.String(),
				Headers:   actualHeaders.String(),
				Body:      cfg.Body,
				Timestamp: timestamp,
				Auth:      authInfo,
			})
		})
	}

	results, stats := runLoadTest(cfg, func(p float64) {
		select {
		case progressChan <- p:
		default:
		}
	}, ui.CancelChan, func(partialResults []BenchmarkResult, partialStats BenchmarkStats) {
		fyne.Do(func() {
			ui.ChartWidget.SetData(partialResults)
			ui.AvgBind.Set(fmt.Sprintf("%.0f ms", partialStats.Avg))
			ui.MinBind.Set(fmt.Sprintf("%.0f ms", partialStats.Min))
			ui.MaxBind.Set(fmt.Sprintf("%.0f ms", partialStats.Max))
			if partialStats.Total > 0 {
				ui.SuccessBind.Set(fmt.Sprintf("%.2f%%", float64(partialStats.Success)/float64(partialStats.Total)*100))
			}
			ui.StatsContainer.Objects = createAdvancedStatsWidgets(partialStats)
			ui.StatsContainer.Refresh()

			if len(ui.RightContentArea.Objects) == 0 || ui.RightContentArea.Objects[0] != ui.ChartBg {
				ui.RightContentArea.Objects = []fyne.CanvasObject{ui.ChartBg, ui.ChartWidget}
				ui.RightContentArea.Refresh()
			}
		})
	})

	resultChan <- results
	statsChan <- stats
}

func (ui *BenchmarkApp) handleResults(resultChan <-chan []BenchmarkResult, statsChan <-chan BenchmarkStats, count, duration, users int) {
	results := <-resultChan
	stats := <-statsChan

	fyne.Do(func() {
		if count > 1 {
			ui.ChartWidget.SetData(results)
			ui.RightContentArea.Objects = []fyne.CanvasObject{ui.ChartBg, ui.ChartWidget}
			ui.RightContentArea.Refresh()
		}

		if len(results) >= 30 && ui.ChartWidget.GetViewMode() != ViewModeFullScreen && !ui.IsFullScreen {
			go func() {
				time.Sleep(500 * time.Millisecond)
				fyne.Do(func() {
					dialog.ShowConfirm("Pantalla Completa Recomendada",
						"Se detectó un test con muchos datos. ¿Deseas cambiar a vista de pantalla completa para mejor visualización?",
						func(response bool) {
							if response {
								ui.ChartWidget.SetViewMode(ViewModeFullScreen) // Trigger manual logic if needed
							}
						}, ui.Window)
				})
			}()
		}

		ui.AvgBind.Set(fmt.Sprintf("%.0f ms", stats.Avg))
		ui.MinBind.Set(fmt.Sprintf("%.0f ms", stats.Min))
		ui.MaxBind.Set(fmt.Sprintf("%.0f ms", stats.Max))
		ui.SuccessBind.Set(fmt.Sprintf("%.2f%%", float64(stats.Success)/float64(stats.Total)*100))

		ui.StatsContainer.Objects = createAdvancedStatsWidgets(stats)
		ui.StatsContainer.Refresh()

		ui.resetRunState()

		if count > 1 || duration > 0 {
			modeDesc := fmt.Sprintf("%d peticiones", stats.Total)
			if duration > 0 {
				modeDesc = fmt.Sprintf("%d segundos - %d peticiones realizadas", duration, stats.Total)
			}
			summary := fmt.Sprintf("Test completado:\n\n%s\nUsuarios concurrentes: %d\nSuccessful: %d (%.1f%%)\nFailed: %d\nAvg response: %.1f ms\nRequests/sec: %.1f",
				modeDesc, users, stats.Success, float64(stats.Success)/float64(stats.Total)*100,
				stats.Total-stats.Success, stats.Avg, stats.RequestsPerSecond)
			dialog.ShowInformation("Benchmark Completado", summary, ui.Window)
		} else {
			dialog.ShowInformation("Request Completado", fmt.Sprintf("Status: %d\nDuration: %.2f ms", results[0].Status, results[0].Duration), ui.Window)
		}
	})
}

func (ui *BenchmarkApp) resetRunState() {
	ui.RunBtn.SetText("Ejecutar Request")
	ui.RunBtn.SetIcon(theme.MediaPlayIcon())
	ui.RunBtn.Enable()
	ui.IsRunning = false
	ui.ProgressBar.Hide()
}

func (ui *BenchmarkApp) buildLayout() {
	// Setup Tree Data
	ui.TreeData = make(map[string]PostmanItem)
	ui.TreeRoots = []string{}

	ui.PostmanTree = widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			if id == "" {
				return ui.TreeRoots
			}
			item := ui.TreeData[id]
			children := []string{}
			for i, child := range item.Items {
				childID := id + "/" + child.Name + strconv.Itoa(i)
				ui.TreeData[childID] = child
				children = append(children, childID)
			}
			return children
		},
		func(id widget.TreeNodeID) bool {
			return len(ui.TreeData[id].Items) > 0
		},
		func(branch bool) fyne.CanvasObject {
			icon := theme.FileTextIcon()
			if branch {
				icon = theme.FolderIcon()
			}
			return container.NewHBox(widget.NewIcon(icon), widget.NewLabel("Template"))
		},
		func(id widget.TreeNodeID, branch bool, o fyne.CanvasObject) {
			lbl := o.(*fyne.Container).Objects[1].(*widget.Label)
			lbl.SetText(ui.TreeData[id].Name)
		},
	)

	ui.PostmanTree.OnSelected = func(id widget.TreeNodeID) {
		item := ui.TreeData[id]
		if item.Request != nil {
			ui.UrlEntry.SetText(item.Request.Url.Raw)
			ui.MethodSelect.SetSelected(item.Request.Method)
			hStr := ""
			for _, h := range item.Request.Header {
				hStr += fmt.Sprintf("%s: %s\n", h.Key, h.Value)
			}
			ui.HeadersEntry.SetText(hStr)
			ui.BodyEntry.SetText(item.Request.Body.Raw)
		}
	}

	leftPanel := container.NewBorder(
		container.NewVBox(ui.ImportBtn, ui.CurlBtn, widget.NewSeparator()),
		nil, nil, nil,
		ui.PostmanTree,
	)

	topBar := container.NewBorder(
		nil, nil,
		container.NewHBox(
			widget.NewLabelWithStyle("🔧 Método:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			ui.MethodSelect,
			widget.NewSeparator(),
			widget.NewLabelWithStyle("⏱️ Modo:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			ui.TestModeSelect,
			widget.NewSeparator(),
			ui.ValueContainer,
			widget.NewSeparator(),
			widget.NewLabelWithStyle("👥 Usuarios:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			ui.UsersEntry,
		),
		container.NewHBox(ui.RunBtn),
		ui.UrlEntry,
	)

	authCard := container.NewVBox(
		container.NewHBox(widget.NewLabelWithStyle("• Autenticación HMAC", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})),
		container.NewGridWithColumns(2, ui.UserEntry, ui.SecretEntry),
	)
	authBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	authSection := container.NewStack(authBg, container.NewPadded(authCard))

	headersCard := container.NewVBox(
		container.NewHBox(widget.NewLabelWithStyle("• Headers", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), widget.NewLabel("(uno por línea)")),
		ui.HeadersEntry,
	)
	headersBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	headersSection := container.NewStack(headersBg, container.NewPadded(headersCard))

	bodyScroll := container.NewScroll(ui.BodyEntry)
	bodyScroll.SetMinSize(fyne.NewSize(400, 300))
	bodyCard := container.NewVBox(
		container.NewBorder(
			nil, nil,
			container.NewHBox(widget.NewLabelWithStyle("• Body", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), widget.NewLabel("(JSON, XML, etc.)")),
			ui.FormatBtn,
			nil,
		),
		bodyScroll,
	)
	bodyBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	bodySection := container.NewStack(bodyBg, container.NewPadded(bodyCard))

	formPanel := container.NewVBox(
		container.NewPadded(widget.NewLabelWithStyle("⚙️ Configuración Request", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})),
		widget.NewSeparator(),
		authSection, widget.NewLabel(""),
		headersSection, widget.NewLabel(""),
		bodySection,
	)
	formScroll := container.NewVScroll(formPanel)
	formScroll.SetMinSize(fyne.NewSize(450, 600))

	// View Controls
	normalViewBtn := widget.NewButtonWithIcon("Vista Normal", theme.HomeIcon(), func() {
		ui.ChartWidget.SetViewMode(ViewModeNormal)
		if ui.IsFullScreen {
			ui.Window.SetContent(ui.OriginalCont)
			ui.IsFullScreen = false
			ui.Window.Resize(fyne.NewSize(1000, 700))
		}
	})
	realTimeViewBtn := widget.NewButtonWithIcon("Tiempo Real", theme.MediaPlayIcon(), func() {
		ui.ChartWidget.SetViewMode(ViewModeRealTime)
	})
	fullScreenBtn := widget.NewButtonWithIcon("Pantalla Completa", theme.ViewFullScreenIcon(), func() {
		ui.ChartWidget.SetViewMode(ViewModeFullScreen)
		if !ui.IsFullScreen {
			ui.OriginalCont = ui.Window.Content()
			ui.IsFullScreen = true
			fullScreenChart := container.NewStack(
				canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255}),
				ui.ChartWidget,
			)
			exitBtn := widget.NewButtonWithIcon("Salir Pantalla Completa", theme.CancelIcon(), func() {
				ui.Window.SetContent(ui.OriginalCont)
				ui.IsFullScreen = false
				ui.ChartWidget.SetViewMode(ViewModeNormal)
				ui.Window.Resize(fyne.NewSize(1000, 700))
			})
			fullScreenContent := container.NewBorder(
				container.NewHBox(exitBtn, widget.NewSeparator()),
				nil, nil, nil,
				fullScreenChart,
			)
			ui.Window.SetContent(fullScreenContent)
			ui.Window.Resize(fyne.NewSize(1400, 900))
		}
	})

	viewControlsContainer := container.NewHBox(
		widget.NewLabel("Vista:"),
		normalViewBtn, realTimeViewBtn, fullScreenBtn,
		widget.NewSeparator(),
	)

	rightPanel := container.NewBorder(
		container.NewVBox(
			container.NewPadded(widget.NewLabelWithStyle("📊 Reporte Estadístico", fyne.TextAlignCenter, fyne.TextStyle{Bold: true})),
			ui.StatsContainer,
			widget.NewSeparator(),
			container.NewPadded(viewControlsContainer),
		),
		nil, nil, nil,
		ui.RightContentArea,
	)

	configAndChart := container.NewHSplit(formScroll, container.NewPadded(rightPanel))
	configAndChart.SetOffset(0.40)

	mainSplit := container.NewHSplit(container.NewPadded(container.NewVScroll(leftPanel)), configAndChart)
	mainSplit.SetOffset(0.20)

	mainContent := container.NewBorder(
		container.NewVBox(topBar, ui.ProgressBar, ui.ConsoleToggleBtn, ui.ConsoleContainer, widget.NewSeparator()),
		nil, nil, nil,
		mainSplit,
	)

	ui.Window.SetContent(mainContent)
}

// ======================================================================================
// SECCIÓN 5: ESTADÍSTICAS UI HELPERS
// ======================================================================================

func createStatsWidgets(avg, min, max, success binding.String, total int) []fyne.CanvasObject {
	makeCell := func(title string, valBinding binding.String, total int, isErrorCheck bool) *fyne.Container {
		lblTitle := widget.NewLabel(title)
		lblTitle.Alignment = fyne.TextAlignCenter
		lblTitle.TextStyle = fyne.TextStyle{Bold: true}

		lblVal := widget.NewLabelWithData(valBinding)
		lblVal.Alignment = fyne.TextAlignCenter

		bgCol := color.NRGBA{R: 40, G: 40, B: 40, A: 255}

		if !isErrorCheck {
			bgCol = color.NRGBA{R: 0, G: 60, B: 0, A: 255}
		} else {
			successStr, _ := success.Get()
			if strings.Contains(successStr, fmt.Sprintf("/%d", total)) && total > 0 {
				if strings.Split(successStr, "/")[0] == fmt.Sprint(total) {
					bgCol = color.NRGBA{R: 0, G: 60, B: 0, A: 255}
				} else {
					bgCol = color.NRGBA{R: 60, G: 0, B: 0, A: 255}
				}
			}
		}

		rect := canvas.NewRectangle(bgCol)
		return container.NewStack(rect, container.NewVBox(lblTitle, lblVal))
	}

	return []fyne.CanvasObject{
		makeCell("Promedio", avg, 0, false),
		makeCell("Mínimo", min, 0, false),
		makeCell("Máximo", max, 0, false),
		makeCell("Éxito", success, total, true),
	}
}

func createAdvancedStatsWidgets(stats BenchmarkStats) []fyne.CanvasObject {
	makeAdvancedCell := func(title string, value string, bgColor color.NRGBA) *fyne.Container {
		txtTitle := canvas.NewText(title, color.White)
		txtTitle.Alignment = fyne.TextAlignCenter
		txtTitle.TextStyle = fyne.TextStyle{Bold: true}
		txtTitle.TextSize = 9

		txtVal := canvas.NewText(value, color.White)
		txtVal.Alignment = fyne.TextAlignCenter
		txtVal.TextStyle = fyne.TextStyle{Bold: true}
		txtVal.TextSize = 11

		rect := canvas.NewRectangle(bgColor)
		content := container.NewVBox(txtTitle, txtVal)
		padded := container.New(&compactPaddingLayout{padding: 3}, content)
		return container.NewStack(rect, padded)
	}

	successRate := float64(stats.Success) / float64(stats.Total) * 100
	errorRate := 100 - successRate

	goodColor := color.NRGBA{R: 0, G: 100, B: 0, A: 255}
	warningColor := color.NRGBA{R: 100, G: 100, B: 0, A: 255}
	errorColor := color.NRGBA{R: 100, G: 0, B: 0, A: 255}
	neutralColor := color.NRGBA{R: 40, G: 40, B: 40, A: 255}

	avgColor := goodColor
	if stats.Avg > 500 {
		avgColor = errorColor
	} else if stats.Avg > 200 {
		avgColor = warningColor
	}

	successColor := goodColor
	if successRate < 95 {
		successColor = errorColor
	} else if successRate < 99 {
		successColor = warningColor
	}

	errorRateColor := goodColor
	if errorRate > 1 {
		errorRateColor = errorColor
	}

	return []fyne.CanvasObject{
		makeAdvancedCell("Total requests", fmt.Sprintf("%d", stats.Total), neutralColor),
		makeAdvancedCell("Requests/second", fmt.Sprintf("%.1f", stats.RequestsPerSecond), neutralColor),
		makeAdvancedCell("Avg response time", fmt.Sprintf("%.0f ms", stats.Avg), avgColor),
		makeAdvancedCell("P90", fmt.Sprintf("%.0f ms", stats.P90), neutralColor),
		makeAdvancedCell("P95", fmt.Sprintf("%.0f ms", stats.P95), neutralColor),
		makeAdvancedCell("P99", fmt.Sprintf("%.0f ms", stats.P99), neutralColor),
		makeAdvancedCell("Min response", fmt.Sprintf("%.0f ms", stats.Min), goodColor),
		makeAdvancedCell("Max response", fmt.Sprintf("%.0f ms", stats.Max), warningColor),
		makeAdvancedCell("Success rate", fmt.Sprintf("%.2f%%", successRate), successColor),
		makeAdvancedCell("Error rate", fmt.Sprintf("%.2f%%", errorRate), errorRateColor),
	}
}

// ======================================================================================
// MAIN ENTRY POINT
// ======================================================================================

func main() {
	// Inicializar y ejecutar la aplicación encapsulada
	myBenchmarkApp := NewBenchmarkApp()
	myBenchmarkApp.setupUI()
	myBenchmarkApp.setupHandlers()
	myBenchmarkApp.buildLayout()
	myBenchmarkApp.Window.ShowAndRun()
}