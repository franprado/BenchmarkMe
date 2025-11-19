# 🚀 BenchmarkPro: Cliente API y Herramienta de Benchmarking

[![Go](https://img.shields.io/badge/Go-1.21+-00ADD8?style=for-the-badge&logo=go)](https://go.dev/)
[![GUI](https://img.shields.io/badge/GUI-Fyne-0077D8?style=for-the-badge&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA1MTIgNTEyIj48ZGVmcz48c3R5bGU+LmNscy0xe2ZpbGw6IzAwYWRkODt9PC9zdHlsZT48L2RlZnM+PHBhdGggY2xhc3M9ImNscy0xIiBkPSJNNDcxLjg1LDE1NC4zMWE4My41NSw4My41NSwwLDAsMC0xMjIuMzctMTkuNTVMMjU2LDIyOC42NSwxNjIuNTIsMTM0Ljc2YTgzLjU1LDgzLjU1LDAsMCwwLTEyMi4zNywxOS41NUE4NCw4NCwwLDAsMCwzNC4zMSwyNTJsMjIxLjY5LDIxMS42OWE1Mi40MSw1Mi40MSwwLDAsMCw3NC4wOCwwTDI0Mi4xNSwyNzcuNDZhMjEuNTksMjEuNTksMCwwLDAtMzAuNTQtMzAuNTRMMTguOTMsMTg1LjQ1YTM2Ljg4LDM2Ljg4LDAsMCwxLDU0LjYtNTguNDdMMjU2LDI5OS45M2wxODguNDgtMTcxLjg5YTM2Ljg4LDM2Ljg4LDAsMCwxLDU0LjYsNTguNDdaIi8+PC9zdmc+)](https://fyne.io/)
[![License](https://img.shields.io/badge/License-MIT-blue.svg?style=for-the-badge)](LICENSE)

**BenchmarkPro** es una aplicación de escritorio multiplataforma desarrollada en **Go (Golang)** utilizando la librería GUI **Fyne**. Combina la funcionalidad esencial de un cliente API (similar a Postman) con una robusta herramienta de prueba de carga y benchmarking, diseñada para evaluar el rendimiento de APIs en tiempo real.

Este proyecto es una pieza clave de mi portafolio profesional, desarrollo de software con Go, manejo de concurrencia, cálculo de estadísticas avanzadas de rendimiento (percentiles, RPS) y creación de interfaces gráficas funcionales y responsivas.

## ✨ Características Principales

### 1. Cliente API (Simulador de Postman)

* **Configuración Completa:** Define el método (`GET`, `POST`, etc.), URL, y `Body` de la request.
* **Gestión de Headers:** Edición de *headers* por separado.
* **Autenticación HMAC:** Soporte incorporado para la generación de firmas HMAC/SHA256 con *User ID* y *Secret Key* para peticiones seguras.
* **Importación cURL:** Analiza y carga peticiones directamente desde comandos cURL.
* **Visualización de Respuesta:** Muestra el cuerpo de la respuesta, el *status code* y la duración de la latencia para peticiones unitarias.

### 2. Herramienta de Benchmarking y Prueba de Carga

* **Modos de Test:** Ejecución por **Cantidad de peticiones** (ej. 100) o por **Duración** (ej. 5 minutos).
* **Concurrencia:** Control total sobre el número de **Usuarios Concurrentes** (`goroutines`) para simular carga real.
* **Gráficos Interactivos Avanzados:** Gráfico de rendimiento que visualiza tres métricas clave simultáneamente:
    * **Latencia Promedio** (Eje principal)
    * **Peticiones por Segundo (RPS)**
    * **Tasa de Error (%)**
* **Estadísticas en Tiempo Real:** Visualización de métricas clave (Avg, Min, Max, P90, P95, P99) actualizadas en tiempo real.
* **Modos de Vista:** **Normal**, **Tiempo Real**, y **Pantalla Completa** para un análisis detallado.
* **Consola de Request:** Muestra los detalles exactos de la petición enviada antes de la ejecución del test.

## 🛠️ Tecnologías Utilizadas

* **Lenguaje:** Go (Golang)
* **Framework GUI:** [Fyne](https://fyne.io/) (para crear la interfaz de usuario nativa y multiplataforma).
* **Concurrencia:** Uso intensivo de `goroutines` y `sync.WaitGroup` para gestionar la ejecución paralela de la prueba de carga.
* **Criptografía:** `crypto/hmac` y `crypto/sha256` para la autenticación.

## 📦 Instalación y Ejecución

Para construir y ejecutar **BenchmarkPro**, necesitas tener instalado Go (versión 1.21 o superior) en tu sistema.

1.  **Clonar el repositorio:**
    ```bash
    git clone [(https://github.com/franprado/BenchmarkMe.git)]
    cd BenchmarkPro
    ```

## 📜 Licencia

Este proyecto está liberado bajo la licencia **MIT**, permitiendo su uso, copia y modificación. Se requiere incluir el aviso de copyright original en cualquier distribución. Para más detalles, consulta el archivo [LICENSE](LICENSE).
