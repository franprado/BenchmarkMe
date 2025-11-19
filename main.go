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

// --- ESTRUCTURAS POSTMAN (Simplificado v2.1) ---

// RequestDetails almacena los detalles de la última request para la consola
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
	Request *PostmanRequest `json:"request,omitempty"` // Si es nil, es carpeta
	Items   []PostmanItem   `json:"item,omitempty"`    // Sub-items (carpetas)
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

// --- ESTRUCTURAS BENCHMARK ---

type BenchmarkResult struct {
	Seq       int     // Número de secuencia
	Timestamp string  // Hora de la petición (Eje X)
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
	Duration        int // Duración en segundos (0 = usar Count)
	ConcurrentUsers int // Número de usuarios concurrentes
}

type BenchmarkStats struct {
	Avg, Min, Max, P90, P95, P99 float64
	Success, Total, ErrorRate    int
	RequestsPerSecond            float64
	TotalDuration                float64
}

// --- CHART WIDGET RESPONSIVO (CORE VISUAL) ---

const MaxVisiblePointsNormal = 10   // Límite óptimo de puntos en vista normal
const MaxVisiblePointsRealTime = 50 // Límite en vista tiempo real
const FullScreenThreshold = 15      // Cambiar a pantalla completa después de este número de puntos

// Modos de vista del gráfico
type ViewMode int

const (
	ViewModeNormal ViewMode = iota
	ViewModeRealTime
	ViewModeFullScreen
)

// PointInfo contiene información de un punto del gráfico
type PointInfo struct {
	X, Y      float32
	Result    BenchmarkResult
	ExtraData string // Información adicional calculada
}

type ChartWidget struct {
	widget.BaseWidget
	Data             []BenchmarkResult
	tooltip          *widget.Label
	tooltipBg        *canvas.Rectangle
	tooltipContainer *fyne.Container
	points           []PointInfo // Información de puntos para hover
	lastMousePos     fyne.Position
	hoverTimeout     *time.Timer
	viewMode         ViewMode
	startTime        time.Time
	lastUpdateTime   time.Time
	parent           *fyne.Container // Referencia al contenedor padre para cambio de modo
}

func NewChartWidget() *ChartWidget {
	c := &ChartWidget{}
	c.ExtendBaseWidget(c)
	c.viewMode = ViewModeNormal
	c.startTime = time.Now()

	// Crear tooltip
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
	c.points = nil // Reset puntos para recalcular
	c.lastUpdateTime = time.Now()

	// Cambiar modo de vista automáticamente basado en cantidad de datos
	if len(d) >= FullScreenThreshold && c.viewMode == ViewModeNormal {
		c.viewMode = ViewModeRealTime
		// Si hay muchos puntos, sugerir pantalla completa
		if len(d) >= 30 {
			c.viewMode = ViewModeFullScreen
		}
	}

	c.Refresh() // Fuerza el repintado
}

// SetViewMode permite cambiar el modo de vista manualmente
func (c *ChartWidget) SetViewMode(mode ViewMode) {
	c.viewMode = mode
	c.Refresh()
}

// GetViewMode retorna el modo actual
func (c *ChartWidget) GetViewMode() ViewMode {
	return c.viewMode
}

func (c *ChartWidget) CreateRenderer() fyne.WidgetRenderer {
	return &chartRenderer{chart: c}
}

// Implementar desktop.Hoverable para detectar movimiento del mouse
func (c *ChartWidget) MouseIn(event *desktop.MouseEvent) {
	c.updateTooltip(event.Position)
}

func (c *ChartWidget) MouseMoved(event *desktop.MouseEvent) {
	c.updateTooltip(event.Position)
}

func (c *ChartWidget) MouseOut() {
	c.hideTooltip()
}

// Actualizar tooltip basado en la posición del mouse
func (c *ChartWidget) updateTooltip(pos fyne.Position) {
	c.lastMousePos = pos

	// Cancelar timeout anterior si existe
	if c.hoverTimeout != nil {
		c.hoverTimeout.Stop()
	}

	// Buscar punto cercano
	for _, point := range c.points {
		// Verificar si el mouse está cerca del punto (radio de 15px)
		dx := pos.X - point.X
		dy := pos.Y - point.Y
		distance := dx*dx + dy*dy

		if distance <= 225 { // 15px de radio (15^2 = 225)
			c.showTooltip(point, pos)
			return
		}
	}

	// Si no hay punto cercano, ocultar después de un delay
	c.hoverTimeout = time.AfterFunc(100*time.Millisecond, func() {
		fyne.Do(func() {
			c.hideTooltip()
		})
	})
}

func (c *ChartWidget) showTooltip(point PointInfo, mousePos fyne.Position) {
	if c.tooltip == nil {
		return
	}

	// Usar fyne.Do para asegurar que la actualización ocurra en el hilo principal
	fyne.Do(func() {
		// Formatear texto del tooltip
		tooltipText := fmt.Sprintf("Seq: %d\nHora: %s\nLatencia: %.1f ms\nStatus: %d%s",
			point.Result.Seq, point.Result.Timestamp, point.Result.Duration, point.Result.Status, point.ExtraData)

		c.tooltip.SetText(tooltipText)

		// Calcular posición del tooltip (offset para no cubrir el punto)
		tooltipX := mousePos.X + 15
		tooltipY := mousePos.Y - 10

		// Ajustar si se sale del widget
		if tooltipX+150 > c.Size().Width {
			tooltipX = mousePos.X - 165
		}
		if tooltipY < 0 {
			tooltipY = mousePos.Y + 20
		}

		// Redimensionar y posicionar
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

type chartRenderer struct {
	chart   *ChartWidget
	objects []fyne.CanvasObject
}

func (r *chartRenderer) MinSize() fyne.Size {
	return fyne.NewSize(100, 100) // Tamaño mínimo
}

func (r *chartRenderer) Layout(size fyne.Size) {
	// Este método se llama automáticamente cuando la ventana cambia de tamaño
	// Aquí regeneramos el gráfico para que ocupe todo el espacio 'size'
	r.objects = r.generateChartObjects(size)
}

func (r *chartRenderer) Refresh() {
	// Llamado cuando cambian los datos
	r.Layout(r.chart.Size())
	canvas.Refresh(r.chart)
}

func (r *chartRenderer) Objects() []fyne.CanvasObject {
	// Agregar tooltip a los objetos si existe
	if r.chart.tooltipContainer != nil {
		return append(r.objects, r.chart.tooltipContainer)
	}
	return r.objects
}

func (r *chartRenderer) Destroy() {
	// No usamos el receptor 'r' aquí, lo cual es aceptable ya que el método debe existir.
}

// Lógica de dibujo matemático puro (Ahora con múltiples modos de vista)
func (r *chartRenderer) generateChartObjects(size fyne.Size) []fyne.CanvasObject {
	// Determinar qué datos mostrar según el modo de vista
	data := r.chart.Data
	maxPoints := MaxVisiblePointsNormal

	switch r.chart.viewMode {
	case ViewModeNormal:
		maxPoints = MaxVisiblePointsNormal
		if len(data) > maxPoints {
			// Mostrar solo los últimos N puntos
			data = data[len(data)-maxPoints:]
		}
	case ViewModeRealTime:
		maxPoints = MaxVisiblePointsRealTime
		if len(data) > maxPoints {
			// En vista tiempo real, muestrear puntos para mantener fluidez
			step := len(data) / maxPoints
			sampledData := make([]BenchmarkResult, 0, maxPoints)
			for i := 0; i < len(data); i += step {
				sampledData = append(sampledData, data[i])
			}
			// Siempre incluir el último punto
			if len(sampledData) < len(data) {
				sampledData = append(sampledData, data[len(data)-1])
			}
			data = sampledData
		}
	case ViewModeFullScreen:
		maxPoints = MaxVisiblePointsRealTime * 2
		if len(data) > maxPoints {
			// En pantalla completa, más puntos pero con muestreo inteligente
			step := len(data) / maxPoints
			sampledData := make([]BenchmarkResult, 0, maxPoints)
			for i := 0; i < len(data); i += step {
				sampledData = append(sampledData, data[i])
			}
			data = sampledData
		}
	}

	objs := []fyne.CanvasObject{}

	// Ajustar padding según el modo de vista - padding mayor para evitar solapamiento de ejes
	// Padding izquierdo más grande para el eje rojo, derecho para el amarillo
	paddingLeft := float32(70)  // Espacio para eje rojo a la izquierda
	paddingRight := float32(60) // Espacio para eje amarillo a la derecha
	paddingTop := float32(40)
	paddingBottom := float32(40)

	if r.chart.viewMode == ViewModeFullScreen {
		paddingLeft = float32(50)
		paddingRight = float32(50)
		paddingTop = float32(30)
		paddingBottom = float32(30)
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

	// Calcular dimensiones del área del gráfico con paddings asimétricos
	graphW := size.Width - paddingLeft - paddingRight
	graphH := size.Height - paddingTop - paddingBottom

	// --- Cálculos para múltiples métricas ---
	// Calcular rangos Y para tiempo de respuesta
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

	// Calcular estadísticas para las líneas adicionales
	var errorCount float64
	var totalDuration float64
	for _, d := range data {
		totalDuration += d.Duration
		if d.Status >= 400 || d.Status == 0 {
			errorCount++
		}
	} // Escalas para múltiples métricas
	xStep := graphW / float32(len(data)-1)
	yScale := graphH / float32(maxDur)

	// --- Ejes y Etiquetas ---
	axisColor := color.NRGBA{R: 100, G: 100, B: 100, A: 255}

	// Eje X (Base)
	xAxis := canvas.NewLine(axisColor)
	xAxis.StrokeWidth = 2
	xAxis.Position1 = fyne.NewPos(paddingLeft, size.Height-paddingBottom)
	xAxis.Position2 = fyne.NewPos(size.Width-paddingRight, size.Height-paddingBottom)
	objs = append(objs, xAxis)

	// Eje Y (Vertical - Central para tiempo de respuesta)
	yAxis := canvas.NewLine(axisColor)
	yAxis.StrokeWidth = 2
	yAxis.Position1 = fyne.NewPos(paddingLeft, paddingTop)
	yAxis.Position2 = fyne.NewPos(paddingLeft, size.Height-paddingBottom)
	objs = append(objs, yAxis)

	// Etiquetas Eje Y para tiempo (lado izquierdo central)
	drawYLabel := func(val float64, yPos float32, label string) {
		lbl := canvas.NewText(label, axisColor)
		lbl.TextSize = 9
		lbl.Alignment = fyne.TextAlignTrailing
		lbl.Move(fyne.NewPos(paddingLeft-35, yPos-6))
		// Línea guía
		grid := canvas.NewLine(color.NRGBA{R: 60, G: 60, B: 60, A: 100})
		grid.Position1 = fyne.NewPos(paddingLeft, yPos)
		grid.Position2 = fyne.NewPos(size.Width-paddingRight, yPos)
		objs = append(objs, lbl, grid)
	}

	drawYLabel(maxDur, paddingTop, fmt.Sprintf("%.0fms", maxDur))
	drawYLabel(maxDur/2, paddingTop+graphH/2, fmt.Sprintf("%.0fms", maxDur/2))
	drawYLabel(0, size.Height-paddingBottom, "0ms")

	// --- Ejes Y adicionales con colores (amarillo y rojo) ---

	// Calcular máximos para requests/sec y error rate
	maxRequestsPerSec := 120.0 // Máximo esperado
	maxErrorRate := 100.0      // Porcentaje

	// Eje Y para Requests/second (amarillo - derecha)
	requestsAxisColor := color.NRGBA{R: 255, G: 193, B: 7, A: 255}
	requestsAxisX := size.Width - paddingRight
	requestsAxis := canvas.NewLine(requestsAxisColor)
	requestsAxis.StrokeWidth = 2
	requestsAxis.Position1 = fyne.NewPos(requestsAxisX, paddingTop)
	requestsAxis.Position2 = fyne.NewPos(requestsAxisX, size.Height-paddingBottom)
	objs = append(objs, requestsAxis)

	// Etiquetas para requests/sec (amarillo - a la derecha del eje)
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

	// Eje Y para Error rate (rojo - extremo izquierdo)
	errorAxisColor := color.NRGBA{R: 237, G: 28, B: 36, A: 255}
	// Posición fija: 15px desde el borde izquierdo (siempre dentro del espacio reservado)
	errorAxisX := float32(15)
	errorAxis := canvas.NewLine(errorAxisColor)
	errorAxis.StrokeWidth = 2
	errorAxis.Position1 = fyne.NewPos(errorAxisX, paddingTop)
	errorAxis.Position2 = fyne.NewPos(errorAxisX, size.Height-paddingBottom)
	objs = append(objs, errorAxis)

	// Etiquetas para error rate (rojo - a la derecha del eje rojo)
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

	// Escalas para cada métrica
	requestsScale := graphH / float32(maxRequestsPerSec)
	errorScale := graphH / float32(maxErrorRate)

	// --- LÍNEAS DE DATOS MÚLTIPLES ---

	// Limpiar puntos para el hover
	r.chart.points = nil

	// Colores basados en la imagen de referencia
	responseTimeColor := color.NRGBA{R: 0, G: 162, B: 232, A: 255} // Azul (Avg response)
	requestsSecColor := color.NRGBA{R: 255, G: 193, B: 7, A: 255}  // Amarillo (Requests/second)
	errorRateColor := color.NRGBA{R: 237, G: 28, B: 36, A: 255}    // Rojo (Error rate)

	var prevResponsePos, prevRequestsPos, prevErrorPos fyne.Position

	// Ajustar grosor de línea y tamaño de puntos según el modo
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

		// Posiciones para cada métrica
		responseY := (size.Height - paddingBottom) - (float32(d.Duration) * yScale)

		// Calcular requests/second
		requestsPerSec := float64(i+1) / (float64(i+1) * 0.1)
		if requestsPerSec > 120 {
			requestsPerSec = 120
		}
		// Usar escala específica de requests
		requestsY := (size.Height - paddingBottom) - (float32(requestsPerSec) * requestsScale)

		// Error rate acumulativo
		errorsUpToNow := float64(0)
		for j := 0; j <= i; j++ {
			if data[j].Status >= 400 || data[j].Status == 0 {
				errorsUpToNow++
			}
		}
		currentErrorRate := (errorsUpToNow / float64(i+1)) * 100
		// Usar escala específica de error rate
		errorY := (size.Height - paddingBottom) - (float32(currentErrorRate) * errorScale)

		// Posiciones actuales
		responsePos := fyne.NewPos(x, responseY)
		requestsPos := fyne.NewPos(x, requestsY)
		errorPos := fyne.NewPos(x, errorY)

		if i > 0 {
			// Línea tiempo de respuesta (azul)
			responseLine := canvas.NewLine(responseTimeColor)
			responseLine.StrokeWidth = lineWidth
			responseLine.Position1 = prevResponsePos
			responseLine.Position2 = responsePos
			objs = append(objs, responseLine)

			// Línea requests/second (amarillo)
			requestsLine := canvas.NewLine(requestsSecColor)
			requestsLine.StrokeWidth = lineWidth
			requestsLine.Position1 = prevRequestsPos
			requestsLine.Position2 = requestsPos
			objs = append(objs, requestsLine)

			// Línea error rate (rojo)
			errorLine := canvas.NewLine(errorRateColor)
			errorLine.StrokeWidth = lineWidth
			errorLine.Position1 = prevErrorPos
			errorLine.Position2 = errorPos
			objs = append(objs, errorLine)
		}

		// Puntos para cada línea (solo en vista normal y tiempo real, no en pantalla completa para mejor rendimiento)
		if r.chart.viewMode != ViewModeFullScreen {
			// Punto tiempo de respuesta (azul)
			responseDot := canvas.NewCircle(responseTimeColor)
			responseDot.Resize(fyne.NewSize(pointSize, pointSize))
			responseDot.Move(fyne.NewPos(x-pointSize/2, responseY-pointSize/2))
			objs = append(objs, responseDot)

			// NO agregar puntos para requests/second (línea amarilla) - solo línea continua

			// Punto error rate (rojo) con etiqueta de porcentaje
			errorDot := canvas.NewCircle(errorRateColor)
			errorDot.Resize(fyne.NewSize(pointSize+2, pointSize+2)) // Ligeramente más grande
			errorDot.Move(fyne.NewPos(x-(pointSize+2)/2, errorY-(pointSize+2)/2))
			objs = append(objs, errorDot)

			// Etiqueta de porcentaje en el punto rojo
			if currentErrorRate > 0 { // Solo mostrar si hay errores
				errorLabel := canvas.NewText(fmt.Sprintf("%.1f%%", currentErrorRate), errorRateColor)
				errorLabel.TextSize = 8
				errorLabel.Alignment = fyne.TextAlignCenter
				errorLabel.Move(fyne.NewPos(x-12, errorY-18)) // Encima del punto
				objs = append(objs, errorLabel)
			}
		}

		// Etiqueta eje X (adaptada según modo de vista)
		lblText := fmt.Sprintf("#%d", d.Seq)
		showLabel := false

		switch r.chart.viewMode {
		case ViewModeNormal:
			// En modo normal, mostrar todos si hay pocos, o cada N si hay muchos
			if len(data) <= 5 {
				lblText = d.Timestamp
				showLabel = true
			} else {
				showLabel = i%2 == 0 || i == len(data)-1
			}
		case ViewModeRealTime:
			// En tiempo real, mostrar menos etiquetas
			showLabel = i%5 == 0 || i == len(data)-1
		case ViewModeFullScreen:
			// En pantalla completa, mostrar aún menos etiquetas
			showLabel = i%10 == 0 || i == len(data)-1
			lblText = d.Timestamp // Mostrar tiempo en lugar de secuencia
		}

		if showLabel {
			xLbl := canvas.NewText(lblText, axisColor)
			xLbl.TextSize = 9
			xLbl.Alignment = fyne.TextAlignCenter
			xLbl.Move(fyne.NewPos(x-15, size.Height-paddingBottom+5))
			objs = append(objs, xLbl)
		}

		// Botones de detalle para cada métrica (solo en modo normal para mejor rendimiento)
		if r.chart.viewMode == ViewModeNormal {
			win := fyne.CurrentApp().Driver().AllWindows()[0]

			// Botón para Avg Response (azul)
			responseInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Avg Response\n\nSeq: %d\nHora: %s\nLatencia: %.2f ms\nStatus: %d\nRequests/sec: %.1f\nError rate: %.1f%%\nTiempo transcurrido: %.1fs",
				d.Seq, d.Timestamp, d.Duration, d.Status, requestsPerSec, currentErrorRate, float64(i+1)*0.1)
			responseBtn := widget.NewButton("", nil)
			responseBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Avg Response", responseInfoTxt, win) }
			responseBtn.Resize(fyne.NewSize(15, 15))
			responseBtn.Move(fyne.NewPos(x-7, responseY-7))
			responseBtn.Importance = widget.LowImportance
			objs = append(objs, responseBtn)

			// Botón para Requests/sec (amarillo)
			requestsInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Requests/Second\n\nSeq: %d\nHora: %s\nRequests/sec: %.1f\nLatencia: %.2f ms\nStatus: %d\nError rate: %.1f%%",
				d.Seq, d.Timestamp, requestsPerSec, d.Duration, d.Status, currentErrorRate)
			requestsBtn := widget.NewButton("", nil)
			requestsBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Requests/Second", requestsInfoTxt, win) }
			requestsBtn.Resize(fyne.NewSize(15, 15))
			requestsBtn.Move(fyne.NewPos(x-7, requestsY-7))
			requestsBtn.Importance = widget.LowImportance
			objs = append(objs, requestsBtn)

			// Botón para Error rate (rojo)
			errorInfoTxt := fmt.Sprintf("DETALLE COMPLETO - Error Rate\n\nSeq: %d\nHora: %s\nError rate: %.1f%%\nErrores acumulados: %.0f de %d\nLatencia: %.2f ms\nStatus: %d",
				d.Seq, d.Timestamp, currentErrorRate, errorsUpToNow, i+1, d.Duration, d.Status)
			errorBtn := widget.NewButton("", nil)
			errorBtn.OnTapped = func() { dialog.ShowInformation("Detalle - Error Rate", errorInfoTxt, win) }
			errorBtn.Resize(fyne.NewSize(15, 15))
			errorBtn.Move(fyne.NewPos(x-7, errorY-7))
			errorBtn.Importance = widget.LowImportance
			objs = append(objs, errorBtn)
		}

		// Guardar información de todos los puntos para hover (siempre, independientemente del modo)
		// Punto azul (response time)
		extraInfo := fmt.Sprintf("\nRequests/sec: %.1f\nError rate: %.1f%%", requestsPerSec, currentErrorRate)
		pointInfoResponse := PointInfo{
			X:         x,
			Y:         responseY,
			Result:    d,
			ExtraData: extraInfo,
		}
		r.chart.points = append(r.chart.points, pointInfoResponse)

		// Punto amarillo (requests/second)
		requestsInfo := fmt.Sprintf("\nRequests/sec: %.1f\nLatencia: %.2f ms\nError rate: %.1f%%", requestsPerSec, d.Duration, currentErrorRate)
		pointInfoRequests := PointInfo{
			X:         x,
			Y:         requestsY,
			Result:    d,
			ExtraData: requestsInfo,
		}
		r.chart.points = append(r.chart.points, pointInfoRequests)

		// Punto rojo (error rate)
		errorInfo := fmt.Sprintf("\nError rate: %.1f%%\nErrores: %.0f de %d\nRequests/sec: %.1f\nLatencia: %.2f ms", currentErrorRate, errorsUpToNow, i+1, requestsPerSec, d.Duration)
		pointInfoError := PointInfo{
			X:         x,
			Y:         errorY,
			Result:    d,
			ExtraData: errorInfo,
		}
		r.chart.points = append(r.chart.points, pointInfoError)

		// Actualizar posiciones previas para las próximas líneas
		prevResponsePos = responsePos
		prevRequestsPos = requestsPos
		prevErrorPos = errorPos
	}

	// Agregar leyenda
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

		// Línea de muestra
		legendLine := canvas.NewLine(item.color)
		legendLine.StrokeWidth = 2
		legendLine.Position1 = fyne.NewPos(legendX, legendY)
		legendLine.Position2 = fyne.NewPos(legendX+15, legendY)
		objs = append(objs, legendLine)

		// Texto de leyenda
		legendText := canvas.NewText(item.text, color.White)
		legendText.TextSize = 9
		legendText.Move(fyne.NewPos(legendX+20, legendY-6))
		objs = append(objs, legendText)
	}

	return objs
}

// --- LÓGICA DE NEGOCIO (Sin cambios en esta sección) ---

func generateHMACSignature(secretKey, message string) string {
	h := hmac.New(sha256.New, []byte(secretKey))
	h.Write([]byte(message))
	return hex.EncodeToString(h.Sum(nil))
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

	// Determinar modo: por tiempo o por cantidad
	useDuration := cfg.Duration > 0
	if useDuration {
		endTime = startTime.Add(time.Duration(cfg.Duration) * time.Second)
	}

	// WaitGroup para sincronizar usuarios concurrentes
	var wg sync.WaitGroup

	// Función que ejecuta requests para un usuario
	executeUser := func(userID int) {
		defer wg.Done()

		client := &http.Client{Timeout: 10 * time.Second}
		requestCount := 0

		for {
			// Verificar cancelación
			select {
			case <-cancelChan:
				return
			default:
			}

			// Verificar condición de parada ANTES de iniciar cualquier request
			if useDuration {
				// Validación estricta: NO iniciar request si el tiempo ha expirado
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

			// Doble verificación para modo por tiempo: asegurar que hay tiempo suficiente
			// para completar la request (estimado 10 segundos de timeout máximo)
			if useDuration && time.Now().Add(10*time.Second).After(endTime) {
				// Si no hay tiempo suficiente para completar la request, terminar
				break
			}

			// Ejecutar request
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

				// Guardar resultado de forma segura
				resultsMutex.Lock()
				totalDuration += duration
				if duration < minDur {
					minDur = duration
				}
				if duration > maxDur {
					maxDur = duration
				}

				requestCount++
				results = append(results, BenchmarkResult{
					Seq:       len(results) + 1,
					Timestamp: start.Format("15:04:05"),
					Duration:  duration,
					Status:    status,
				})

				currentTotal := len(results)

				// Copiar resultados para actualización en tiempo real
				resultsCopy := make([]BenchmarkResult, len(results))
				copy(resultsCopy, results)
				resultsMutex.Unlock()

				// Actualizar progreso
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

				// Actualizar UI en tiempo real (throttle cada 5 requests)
				if realtimeUpdate != nil && currentTotal%5 == 0 {
					// Calcular estadísticas parciales
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

			// Pequeña pausa para no saturar
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Lanzar usuarios concurrentes
	users := cfg.ConcurrentUsers
	if users < 1 {
		users = 1
	}

	for i := 0; i < users; i++ {
		wg.Add(1)
		go executeUser(i)
	}

	// Esperar a que terminen todos los usuarios
	wg.Wait()

	// Calcular percentiles
	resultsMutex.Lock()
	durations := make([]float64, len(results))
	for i, r := range results {
		durations[i] = r.Duration
	}
	resultsMutex.Unlock()

	// Ordenar para percentiles
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

		// Calcular requests/second basado en tiempo real transcurrido
		actualDuration := time.Since(startTime).Seconds()
		stats.RequestsPerSecond = float64(stats.Total) / actualDuration

		// Calcular percentiles
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

// parseCurlCommand extrae información de un comando cURL
func parseCurlCommand(curl string, urlEntry *widget.Entry, methodSelect *widget.Select, headersEntry *widget.Entry, bodyEntry *widget.Entry) {
	curl = strings.TrimSpace(curl)

	// Normalizar saltos de línea primero para facilitar el parsing
	curl = strings.ReplaceAll(curl, "\r\n", " ")
	curl = strings.ReplaceAll(curl, "\n", " ")
	curl = strings.ReplaceAll(curl, "\\", "")

	var foundURL bool

	// Extraer URL - mejorado para manejar URLs con comillas
	if strings.Contains(curl, "curl") {
		// Buscar URL entre comillas primero
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
				// Sin comillas - buscar por espacios
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

	// Extraer método (-X o --request) - SIEMPRE intentar extraer
	var methodFound bool
	if idx := strings.Index(curl, "-X "); idx != -1 {
		fields := strings.Fields(curl[idx:])
		if len(fields) > 1 {
			method := strings.ToUpper(strings.Trim(fields[1], `"'`))
			methodSelect.SetSelected(method)
			methodFound = true
		}
	}

	if !methodFound {
		if idx := strings.Index(curl, "--request "); idx != -1 {
			fields := strings.Fields(curl[idx:])
			if len(fields) > 1 {
				method := strings.ToUpper(strings.Trim(fields[1], `"'`))
				methodSelect.SetSelected(method)
				methodFound = true
			}
		}
	}

	// Si no se encontró método explícito pero hay --data o -d, es POST
	if !methodFound {
		if strings.Contains(curl, "--data") || strings.Contains(curl, "-d ") {
			methodSelect.SetSelected("POST")
		}
	}

	// Extraer headers (-H o --header)
	headers := []string{}

	// Buscar todos los -H y --header
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

			// Determinar el delimitador de cierre
			var endDelimiter string
			if strings.HasSuffix(pattern, "'") {
				endDelimiter = "'"
			} else {
				endDelimiter = `"`
			}

			// Buscar el cierre del header
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

	// Extraer body (-d, --data, --data-raw)
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
} // executeRequest ejecuta un single HTTP request
func executeRequest(cfg RequestConfig, seq int) BenchmarkResult {
	client := &http.Client{Timeout: 10 * time.Second}

	var bodyReader io.Reader
	if cfg.Body != "" {
		bodyReader = strings.NewReader(cfg.Body)
	}

	req, err := http.NewRequest(cfg.Method, cfg.URL, bodyReader)
	if err != nil {
		return BenchmarkResult{Seq: seq, Timestamp: time.Now().Format("15:04:05"), Duration: 0, Status: 0}
	}

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
	}

	return BenchmarkResult{
		Seq:       seq,
		Timestamp: start.Format("15:04:05"),
		Duration:  duration,
		Status:    status,
	}
}

// --- UI PRINCIPAL ---

// compactPaddingLayout es un layout con padding reducido para compactar elementos
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

func main() {
	// CORRECCIÓN: Usamos NewWithID para evitar la advertencia de las preferencias.
	myApp := app.NewWithID("com.francisco.benchmarkpro")
	myWindow := myApp.NewWindow("Benchmark Pro - Postman Integrado")
	myWindow.Resize(fyne.NewSize(1000, 700))

	// --- ELEMENTOS DE UI ---
	urlEntry := widget.NewEntry()
	urlEntry.SetText("https://google.com")
	urlEntry.SetPlaceHolder("https://api...")

	userEntry := widget.NewEntry()
	userEntry.SetPlaceHolder("User ID")
	secretEntry := widget.NewPasswordEntry()
	secretEntry.SetPlaceHolder("HMAC Secret")

	methodSelect := widget.NewSelect([]string{"GET", "POST", "PUT", "DELETE"}, nil)
	methodSelect.Selected = "GET"

	headersEntry := widget.NewMultiLineEntry()
	headersEntry.SetPlaceHolder("Content-Type: application/json\nAuthorization: Bearer token")
	headersEntry.SetMinRowsVisible(4)

	bodyEntry := widget.NewMultiLineEntry()
	bodyEntry.SetPlaceHolder(`{\n  "key": "value",\n  "nested": {\n    "data": "example"\n  }\n}`)
	bodyEntry.SetMinRowsVisible(15) // Más grande para mejor visualización
	bodyEntry.Wrapping = fyne.TextWrapWord

	// Botón para formatear JSON/XML
	formatBtn := widget.NewButtonWithIcon("Formatear Body", theme.DocumentIcon(), func() {
		body := strings.TrimSpace(bodyEntry.Text)
		if body == "" {
			return
		}

		// Intentar formatear como JSON
		var jsonData interface{}
		if err := json.Unmarshal([]byte(body), &jsonData); err == nil {
			formatted, err := json.MarshalIndent(jsonData, "", "  ")
			if err == nil {
				bodyEntry.SetText(string(formatted))
				return
			}
		}

		// Si no es JSON, intentar como XML (básico)
		if strings.HasPrefix(body, "<") {
			// Formateo básico de XML con indentación
			formatted := strings.ReplaceAll(body, "><", ">\n<")
			bodyEntry.SetText(formatted)
			return
		}

		dialog.ShowInformation("Formateo", "No se pudo formatear. Asegúrate de que sea JSON o XML válido.", myWindow)
	})

	// Selector de modo de test
	testModeSelect := widget.NewSelect([]string{"Por Cantidad", "Por Tiempo"}, nil)
	testModeSelect.SetSelected("Por Cantidad")

	countEntry := widget.NewEntry()
	countEntry.SetText("10")
	countEntry.SetPlaceHolder("Ej: 10, 50, 100")

	durationEntry := widget.NewEntry()
	durationEntry.SetText("1")
	durationEntry.SetPlaceHolder("Ej: 1, 5, 10")
	durationEntry.Hide()

	// Selector de unidad de tiempo
	timeUnitSelect := widget.NewSelect([]string{"Segundos", "Minutos", "Horas"}, nil)
	timeUnitSelect.SetSelected("Minutos")
	timeUnitSelect.Hide()

	usersEntry := widget.NewEntry()
	usersEntry.SetText("1")
	usersEntry.SetPlaceHolder("Usuarios concurrentes")

	// Contenedor dinámico para cantidad/duración con unidad de tiempo
	durationWithUnit := container.NewHBox(durationEntry, timeUnitSelect)
	valueContainer := container.NewStack(countEntry, durationWithUnit)

	// Cambiar UI según el modo seleccionado
	testModeSelect.OnChanged = func(mode string) {
		if mode == "Por Tiempo" {
			countEntry.Hide()
			durationEntry.Show()
			timeUnitSelect.Show()
			valueContainer.Refresh()
		} else {
			durationEntry.Hide()
			timeUnitSelect.Hide()
			countEntry.Show()
			valueContainer.Refresh()
		}
	}

	// Consola desplegable para mostrar detalles de la request
	consoleEntry := widget.NewMultiLineEntry()
	consoleEntry.Wrapping = fyne.TextWrapWord
	consoleEntry.SetMinRowsVisible(10)

	// Estilo personalizado para que se vea como consola pero sea seleccionable
	consoleBg := canvas.NewRectangle(color.NRGBA{R: 20, G: 20, B: 25, A: 255})

	consoleDisplay := container.NewStack(consoleBg, container.NewPadded(consoleEntry))

	consoleVisible := false
	consoleContainer := container.NewVBox()
	consoleContainer.Hide()

	consoleToggleBtn := widget.NewButtonWithIcon("Mostrar Consola Request", theme.InfoIcon(), nil)
	consoleToggleBtn.OnTapped = func() {
		if consoleVisible {
			consoleContainer.Hide()
			consoleToggleBtn.SetText("Mostrar Consola Request")
			consoleToggleBtn.SetIcon(theme.InfoIcon())
			consoleVisible = false
		} else {
			consoleContainer.Show()
			consoleToggleBtn.SetText("Ocultar Consola Request")
			consoleToggleBtn.SetIcon(theme.ContentClearIcon())
			consoleVisible = true
		}
	}

	updateConsole := func(details RequestDetails) {
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
		consoleEntry.SetText(consoleText)
	}

	consoleScrollContainer := container.NewVScroll(consoleDisplay)
	consoleScrollContainer.SetMinSize(fyne.NewSize(0, 250))
	consoleContainer.Objects = []fyne.CanvasObject{
		widget.NewSeparator(),
		newBoldLabel("Detalles de la Request Enviada", fyne.TextAlignLeading),
		consoleScrollContainer,
	}

	// --- BINDING PARA ESTADÍSTICAS ---
	avgBind := binding.NewString()
	minBind := binding.NewString()
	maxBind := binding.NewString()
	successBind := binding.NewString()

	avgBind.Set("Promedio: -")
	minBind.Set("Mínimo: -")
	maxBind.Set("Máximo: -")
	successBind.Set("Éxito: -")

	// Variables para el Árbol de Postman
	treeData := make(map[string]PostmanItem)
	treeRoots := []string{}

	var processItems func([]PostmanItem, string)
	processItems = func(items []PostmanItem, parentID string) {
		for i, item := range items {
			id := item.Name + strconv.Itoa(i)
			if parentID != "" {
				id = parentID + "/" + id
			}

			treeData[id] = item
			if parentID == "" {
				treeRoots = append(treeRoots, id)
			}
			if len(item.Items) > 0 {
				processItems(item.Items, id)
			}
		}
	}

	postmanTree := widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			if id == "" {
				return treeRoots
			}
			item := treeData[id]
			children := []string{}
			for i, child := range item.Items {
				childID := id + "/" + child.Name + strconv.Itoa(i)
				treeData[childID] = child
				children = append(children, childID)
			}
			return children
		},
		func(id widget.TreeNodeID) bool {
			return len(treeData[id].Items) > 0
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
			lbl.SetText(treeData[id].Name)
		},
	)

	postmanTree.OnSelected = func(id widget.TreeNodeID) {
		item := treeData[id]
		if item.Request != nil {
			urlEntry.SetText(item.Request.Url.Raw)
			methodSelect.SetSelected(item.Request.Method)

			hStr := ""
			for _, h := range item.Request.Header {
				hStr += fmt.Sprintf("%s: %s\n", h.Key, h.Value)
			}
			headersEntry.SetText(hStr)
			bodyEntry.SetText(item.Request.Body.Raw)
		}
	}

	importBtn := widget.NewButtonWithIcon("Cargar JSON Postman", theme.FolderOpenIcon(), func() {
		fd := dialog.NewFileOpen(func(reader fyne.URIReadCloser, err error) {
			if err != nil || reader == nil {
				return
			}
			defer reader.Close()

			byteValue, _ := io.ReadAll(reader)
			var collection PostmanCollection
			err = json.Unmarshal(byteValue, &collection)
			if err != nil {
				dialog.ShowError(fmt.Errorf("Error al parsear Postman JSON: %w", err), myWindow)
				return
			}

			treeData = make(map[string]PostmanItem)
			treeRoots = []string{}
			processItems(collection.Items, "")
			postmanTree.Refresh()

		}, myWindow)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".json"}))
		fd.Show()
	})

	// Botón para importar desde cURL
	curlBtn := widget.NewButtonWithIcon("Pegar cURL", theme.ContentPasteIcon(), func() {
		curlEntry := widget.NewMultiLineEntry()
		curlEntry.SetPlaceHolder("Pega aquí tu comando cURL completo...")
		curlEntry.SetMinRowsVisible(10)

		// Crear diálogo personalizado con ventana más ancha
		formDialog := dialog.NewForm("Importar desde cURL", "Importar", "Cancelar",
			[]*widget.FormItem{
				widget.NewFormItem("Comando cURL:", curlEntry),
			}, func(ok bool) {
				if !ok || curlEntry.Text == "" {
					return
				}
				parseCurlCommand(curlEntry.Text, urlEntry, methodSelect, headersEntry, bodyEntry)
			}, myWindow)

		formDialog.Resize(fyne.NewSize(800, 400))
		formDialog.Show()
	})

	// --- AREA GRAFICA Y EJECUCION ---

	chartWidget := NewChartWidget()
	progressBar := widget.NewProgressBar()
	progressBar.Hide()

	// Área para mostrar respuesta única
	responseViewer := widget.NewMultiLineEntry()
	responseViewer.SetPlaceHolder("Respuesta del servidor aparecerá aquí...")
	responseViewer.Wrapping = fyne.TextWrapWord

	// Variables para control de vistas
	var isFullScreen bool
	var originalContent fyne.CanvasObject

	// Botones de control de vista
	normalViewBtn := widget.NewButtonWithIcon("Vista Normal", theme.HomeIcon(), func() {
		chartWidget.SetViewMode(ViewModeNormal)
		if isFullScreen {
			myWindow.SetContent(originalContent)
			isFullScreen = false
			myWindow.Resize(fyne.NewSize(1000, 700))
		}
	})

	realTimeViewBtn := widget.NewButtonWithIcon("Tiempo Real", theme.MediaPlayIcon(), func() {
		chartWidget.SetViewMode(ViewModeRealTime)
	})

	fullScreenBtn := widget.NewButtonWithIcon("Pantalla Completa", theme.ViewFullScreenIcon(), func() {
		chartWidget.SetViewMode(ViewModeFullScreen)
		if !isFullScreen {
			originalContent = myWindow.Content()
			isFullScreen = true

			// Crear contenido de pantalla completa
			fullScreenChart := container.NewStack(
				canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255}),
				chartWidget,
			)

			exitBtn := widget.NewButtonWithIcon("Salir Pantalla Completa", theme.CancelIcon(), func() {
				myWindow.SetContent(originalContent)
				isFullScreen = false
				chartWidget.SetViewMode(ViewModeNormal)
				myWindow.Resize(fyne.NewSize(1000, 700))
			})

			fullScreenContent := container.NewBorder(
				container.NewHBox(exitBtn, widget.NewSeparator()),
				nil, nil, nil,
				fullScreenChart,
			)

			myWindow.SetContent(fullScreenContent)
			myWindow.Resize(fyne.NewSize(1400, 900))
		}
	})

	viewControlsContainer := container.NewHBox(
		widget.NewLabel("Vista:"),
		normalViewBtn,
		realTimeViewBtn,
		fullScreenBtn,
		widget.NewSeparator(),
	)

	statsContainer := container.NewGridWithColumns(10) // 10 columnas = 1 fila compacta
	// Inicializar con estadísticas vacías usando las métricas básicas
	statsContainer.Objects = createStatsWidgets(avgBind, minBind, maxBind, successBind, 0)

	// Container dinámico que cambia entre gráfico y respuesta
	var rightContentArea *fyne.Container
	chartBg := canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255})
	rightContentArea = container.NewStack(chartBg, chartWidget)

	runBtn := widget.NewButtonWithIcon("Ejecutar Request", theme.MediaPlayIcon(), nil)

	// Variable para controlar cancelación
	var cancelChan chan bool
	var isRunning bool

	runBtn.OnTapped = func() {
		// Si está ejecutando, cancelar
		if isRunning {
			if cancelChan != nil {
				close(cancelChan)
				runBtn.SetText("Cancelando...")
				runBtn.Disable()
			}
			return
		}

		// Validar URL
		if urlEntry.Text == "" {
			dialog.ShowError(fmt.Errorf("por favor ingresa una URL"), myWindow)
			return
		}

		// Limpiar datos de ejecución anterior
		chartWidget.SetData([]BenchmarkResult{})
		responseViewer.SetText("")

		// Resetear estadísticas
		avgBind.Set("Promedio: -")
		minBind.Set("Mínimo: -")
		maxBind.Set("Máximo: -")
		successBind.Set("Éxito: -")
		statsContainer.Objects = createStatsWidgets(avgBind, minBind, maxBind, successBind, 0)
		statsContainer.Refresh()

		// Cambiar a vista de gráfico
		rightContentArea.Objects = []fyne.CanvasObject{chartBg, chartWidget}
		rightContentArea.Refresh()

		// Cambiar botón a Cancelar
		runBtn.SetText("Cancelar")
		runBtn.SetIcon(theme.CancelIcon())
		isRunning = true
		cancelChan = make(chan bool)
		progressBar.Show()
		progressBar.SetValue(0)

		// Leer configuración según el modo
		count := 1
		duration := 0
		users := 1

		if testModeSelect.Selected == "Por Tiempo" {
			var durationValue int
			fmt.Sscanf(durationEntry.Text, "%d", &durationValue)
			if durationValue <= 0 {
				dialog.ShowError(fmt.Errorf("ingresa una duración válida"), myWindow)
				// Restaurar botón
				runBtn.SetText("Ejecutar Request")
				runBtn.SetIcon(theme.MediaPlayIcon())
				runBtn.Enable()
				isRunning = false
				progressBar.Hide()
				return
			}

			// Convertir a segundos según la unidad seleccionada
			switch timeUnitSelect.Selected {
			case "Minutos":
				duration = durationValue * 60
			case "Horas":
				duration = durationValue * 3600
			default: // Segundos
				duration = durationValue
			}
		} else {
			fmt.Sscanf(countEntry.Text, "%d", &count)
			if count <= 0 {
				dialog.ShowError(fmt.Errorf("ingresa una cantidad válida de peticiones"), myWindow)
				// Restaurar botón
				runBtn.SetText("Ejecutar Request")
				runBtn.SetIcon(theme.MediaPlayIcon())
				runBtn.Enable()
				isRunning = false
				progressBar.Hide()
				return
			}
		}

		fmt.Sscanf(usersEntry.Text, "%d", &users)
		if users <= 0 {
			users = 1
		}

		cfg := RequestConfig{
			URL: urlEntry.Text, Method: methodSelect.Selected,
			Headers: headersEntry.Text, Body: bodyEntry.Text,
			Count: count, Duration: duration, ConcurrentUsers: users,
			User: userEntry.Text, Secret: secretEntry.Text,
		}

		// La consola se actualizará DESPUÉS de construir la request real con todos los headers

		// Usar un canal para comunicación thread-safe
		resultChan := make(chan []BenchmarkResult)
		statsChan := make(chan BenchmarkStats)
		progressChan := make(chan float64)

		// Goroutine para manejar progreso
		go func() {
			for progress := range progressChan {
				fyne.Do(func() {
					progressBar.SetValue(progress)
				})
			}
		}()

		// Ejecutar test en background
		go func() {
			defer close(progressChan)
			defer close(resultChan)
			defer close(statsChan)

			// Si count == 1 Y es modo "Por Cantidad", ejecutar request única y capturar respuesta completa
			if count == 1 && duration == 0 {
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

					// Capturar headers reales construidos
					var actualHeaders strings.Builder
					for name, values := range req.Header {
						for _, value := range values {
							actualHeaders.WriteString(fmt.Sprintf("%s: %s\n", name, value))
						}
					}

					// Actualizar consola con datos reales DESPUÉS de construir la request
					fyne.Do(func() {
						updateConsole(RequestDetails{
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

					// Enviar resultado
					result := BenchmarkResult{
						Seq:       1,
						Timestamp: start.Format("15:04:05"),
						Duration:  duration,
						Status:    status,
					}

					// Guardar responseBody en un canal separado
					responseChan := make(chan string, 1)
					responseChan <- responseBody
					close(responseChan)

					// Actualizar UI
					fyne.Do(func() {
						responseText := fmt.Sprintf("STATUS: %d\nDURATION: %.2f ms\nTIMESTAMP: %s\n\n--- RESPONSE BODY ---\n\n%s",
							status, duration, start.Format("15:04:05"), <-responseChan)
						responseViewer.SetText(responseText)

						// Cambiar a vista de respuesta
						rightContentArea.Objects = []fyne.CanvasObject{
							canvas.NewRectangle(color.NRGBA{R: 25, G: 25, B: 25, A: 255}),
							container.NewScroll(responseViewer),
						}
						rightContentArea.Refresh()
					})

					resultChan <- []BenchmarkResult{result}
					statsChan <- BenchmarkStats{
						Avg:               duration,
						Min:               duration,
						Max:               duration,
						P90:               duration,
						P95:               duration,
						P99:               duration,
						Success:           1,
						Total:             1,
						ErrorRate:         0,
						RequestsPerSecond: 1.0 / (duration / 1000.0),
						TotalDuration:     duration,
					}
				}
			} else {
				// Modo benchmark (múltiples requests)
				// Construir una request de ejemplo para mostrar en consola
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

					// Capturar headers reales
					var actualHeaders strings.Builder
					for name, values := range sampleReq.Header {
						for _, value := range values {
							actualHeaders.WriteString(fmt.Sprintf("%s: %s\n", name, value))
						}
					}

					// Actualizar consola con datos reales
					fyne.Do(func() {
						updateConsole(RequestDetails{
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
				}, cancelChan, func(partialResults []BenchmarkResult, partialStats BenchmarkStats) {
					// Actualizar UI en tiempo real
					fyne.Do(func() {
						chartWidget.SetData(partialResults)

						// Actualizar estadísticas
						avgBind.Set(fmt.Sprintf("%.0f ms", partialStats.Avg))
						minBind.Set(fmt.Sprintf("%.0f ms", partialStats.Min))
						maxBind.Set(fmt.Sprintf("%.0f ms", partialStats.Max))
						if partialStats.Total > 0 {
							successBind.Set(fmt.Sprintf("%.2f%%", float64(partialStats.Success)/float64(partialStats.Total)*100))
						}

						statsContainer.Objects = createAdvancedStatsWidgets(partialStats)
						statsContainer.Refresh()

						// Asegurar que está en vista de gráfico
						if len(rightContentArea.Objects) == 0 || rightContentArea.Objects[0] != chartBg {
							rightContentArea.Objects = []fyne.CanvasObject{chartBg, chartWidget}
							rightContentArea.Refresh()
						}
					})
				})

				resultChan <- results
				statsChan <- stats
			}
		}()

		// Goroutine para actualizar UI cuando lleguen los resultados
		go func() {
			results := <-resultChan
			stats := <-statsChan

			// Usar fyne.Do para actualizar UI en el main thread
			fyne.Do(func() {
				// Solo actualizar gráfico si hay más de 1 request
				if count > 1 {
					chartWidget.SetData(results)

					// Cambiar a vista de gráfico
					rightContentArea.Objects = []fyne.CanvasObject{
						chartBg,
						chartWidget,
					}
					rightContentArea.Refresh()
				}

				// Si hay muchos datos y no estamos en pantalla completa, sugerir el cambio
				if len(results) >= 30 && chartWidget.GetViewMode() != ViewModeFullScreen && !isFullScreen {
					go func() {
						time.Sleep(500 * time.Millisecond) // Esperar un poco antes de mostrar el diálogo
						fyne.Do(func() {
							dialog.ShowConfirm("Pantalla Completa Recomendada",
								"Se detectó un test con muchos datos. ¿Deseas cambiar a vista de pantalla completa para mejor visualización?",
								func(response bool) {
									if response {
										fullScreenBtn.OnTapped() // Activar pantalla completa
									}
								}, myWindow)
						})
					}()
				}

				// Actualizar estadísticas con más detalle
				avgBind.Set(fmt.Sprintf("%.0f ms", stats.Avg))
				minBind.Set(fmt.Sprintf("%.0f ms", stats.Min))
				maxBind.Set(fmt.Sprintf("%.0f ms", stats.Max))
				successBind.Set(fmt.Sprintf("%.2f%%", float64(stats.Success)/float64(stats.Total)*100))

				statsContainer.Objects = createAdvancedStatsWidgets(stats)
				statsContainer.Refresh()

				// Restaurar botón
				runBtn.SetText("Ejecutar Request")
				runBtn.SetIcon(theme.MediaPlayIcon())
				runBtn.Enable()
				isRunning = false
				progressBar.Hide()

				// Mostrar resumen solo si es más de 1 request
				if count > 1 || duration > 0 {
					modeDesc := fmt.Sprintf("%d peticiones", stats.Total)
					if duration > 0 {
						modeDesc = fmt.Sprintf("%d segundos - %d peticiones realizadas", duration, stats.Total)
					}

					summary := fmt.Sprintf("Test completado:\n\n%s\nUsuarios concurrentes: %d\nSuccessful: %d (%.1f%%)\nFailed: %d\nAvg response: %.1f ms\nRequests/sec: %.1f",
						modeDesc, users, stats.Success, float64(stats.Success)/float64(stats.Total)*100,
						stats.Total-stats.Success, stats.Avg, stats.RequestsPerSecond)
					dialog.ShowInformation("Benchmark Completado", summary, myWindow)
				} else {
					dialog.ShowInformation("Request Completado", fmt.Sprintf("Status: %d\nDuration: %.2f ms", results[0].Status, results[0].Duration), myWindow)
				}
			})
		}()
	} // --- ARMADO DE LA PANTALLA ---

	leftPanel := container.NewBorder(
		container.NewVBox(
			importBtn,
			curlBtn,
			widget.NewSeparator(),
		),
		nil, nil, nil,
		postmanTree,
	)

	// Barra superior con URL, método y botón ejecutar (optimiza espacio)
	topBar := container.NewBorder(
		nil, nil,
		container.NewHBox(
			widget.NewLabelWithStyle("🔧 Método:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			methodSelect,
			widget.NewSeparator(),
			widget.NewLabelWithStyle("⏱️ Modo:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			testModeSelect,
			widget.NewSeparator(),
			valueContainer,
			widget.NewSeparator(),
			widget.NewLabelWithStyle("👥 Usuarios:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			usersEntry,
		),
		container.NewHBox(
			runBtn,
		),
		urlEntry,
	)

	// Contenedor de configuración con mejor organización visual
	// Card para Auth
	authCard := container.NewVBox(
		container.NewHBox(
			widget.NewLabelWithStyle("• Autenticación HMAC", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		),
		container.NewGridWithColumns(2, userEntry, secretEntry),
	)
	authBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	authSection := container.NewStack(authBg, container.NewPadded(authCard))

	// Card para Headers
	headersCard := container.NewVBox(
		container.NewHBox(
			widget.NewLabelWithStyle("• Headers", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
			widget.NewLabel("(uno por línea)"),
		),
		headersEntry,
	)
	headersBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	headersSection := container.NewStack(headersBg, container.NewPadded(headersCard))

	// Card para Body con botón de formateo y scroll
	bodyScroll := container.NewScroll(bodyEntry)
	bodyScroll.SetMinSize(fyne.NewSize(400, 300)) // Tamaño mínimo grande

	bodyCard := container.NewVBox(
		container.NewBorder(
			nil, nil,
			container.NewHBox(
				widget.NewLabelWithStyle("• Body", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
				widget.NewLabel("(JSON, XML, etc.)"),
			),
			formatBtn,
			nil,
		),
		bodyScroll,
	)
	bodyBg := canvas.NewRectangle(color.NRGBA{R: 45, G: 45, B: 50, A: 255})
	bodySection := container.NewStack(bodyBg, container.NewPadded(bodyCard))

	formPanel := container.NewVBox(
		container.NewPadded(
			widget.NewLabelWithStyle("⚙️ Configuración Request", fyne.TextAlignCenter, fyne.TextStyle{Bold: true, Italic: false}),
		),
		widget.NewSeparator(),
		authSection,
		widget.NewLabel(""), // Espaciado
		headersSection,
		widget.NewLabel(""), // Espaciado
		bodySection,
	)

	// Envolver en scroll con tamaño mínimo
	formScroll := container.NewVScroll(formPanel)
	formScroll.SetMinSize(fyne.NewSize(450, 600)) // Ancho mínimo para el área de config

	// Panel de Resultados (Derecha) con mejor visual
	rightPanel := container.NewBorder(
		container.NewVBox(
			container.NewPadded(
				widget.NewLabelWithStyle("📊 Reporte Estadístico", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
			),
			statsContainer,
			widget.NewSeparator(),
			container.NewPadded(viewControlsContainer),
		),
		nil, nil, nil,
		rightContentArea,
	)

	// Split principal: Izq (Árbol) - Centro (Config) - Der (Gráfico)
	// El usuario puede ajustar el ancho arrastrando el divisor
	configAndChart := container.NewHSplit(
		formScroll,
		container.NewPadded(rightPanel),
	)
	configAndChart.SetOffset(0.40) // 40% del espacio para configuración, ajustable manualmente

	mainSplit := container.NewHSplit(
		container.NewPadded(container.NewVScroll(leftPanel)),
		configAndChart,
	)
	mainSplit.SetOffset(0.20)

	// Contenedor principal con barra superior, consola desplegable y contenido
	mainContent := container.NewBorder(
		container.NewVBox(
			topBar,
			progressBar,
			consoleToggleBtn,
			consoleContainer,
			widget.NewSeparator(),
		),
		nil, nil, nil,
		mainSplit,
	)

	myWindow.SetContent(mainContent)
	myWindow.ShowAndRun()
}

// createStatsWidgets genera las etiquetas para la tabla de estadísticas
// Recibe los bindings y el total para la lógica de color
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

	// Las llamadas ahora pasan un string simple para el título y el binding para el valor.
	return []fyne.CanvasObject{
		makeCell("Promedio", avg, 0, false),
		makeCell("Mínimo", min, 0, false),
		makeCell("Máximo", max, 0, false),
		makeCell("Éxito", success, total, true),
	}
}

// createAdvancedStatsWidgets genera estadísticas avanzadas como en la imagen
func createAdvancedStatsWidgets(stats BenchmarkStats) []fyne.CanvasObject {
	makeAdvancedCell := func(title string, value string, bgColor color.NRGBA) *fyne.Container {
		// Usar canvas.Text para control de tamaño de fuente
		txtTitle := canvas.NewText(title, color.White)
		txtTitle.Alignment = fyne.TextAlignCenter
		txtTitle.TextStyle = fyne.TextStyle{Bold: true}
		txtTitle.TextSize = 9 // Texto más pequeño para compactar

		txtVal := canvas.NewText(value, color.White)
		txtVal.Alignment = fyne.TextAlignCenter
		txtVal.TextStyle = fyne.TextStyle{Bold: true}
		txtVal.TextSize = 11 // Valor ligeramente más grande que el título

		rect := canvas.NewRectangle(bgColor)
		content := container.NewVBox(txtTitle, txtVal)
		// Padding más pequeño para compactar
		padded := container.New(&compactPaddingLayout{padding: 3}, content)
		return container.NewStack(rect, padded)
	}

	successRate := float64(stats.Success) / float64(stats.Total) * 100
	errorRate := 100 - successRate

	// Colores basados en performance
	goodColor := color.NRGBA{R: 0, G: 100, B: 0, A: 255}
	warningColor := color.NRGBA{R: 100, G: 100, B: 0, A: 255}
	errorColor := color.NRGBA{R: 100, G: 0, B: 0, A: 255}
	neutralColor := color.NRGBA{R: 40, G: 40, B: 40, A: 255}

	// Determinar colores basados en métricas
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
