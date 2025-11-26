package location

import (
	"math"
	"time"

	"github.com/pkg/errors"
	"gonum.org/v1/gonum/mat"
)

const (
	defaultSpeedThreshold         = 1.0 / 3.6  // m/s (1 km/h) - below this, heading doesn't update
	defaultHeadingRampSpeed       = 10.0 / 3.6 // m/s (10 km/h) - above this, full heading responsiveness
	defaultPositionThreshold      = 3.0        // meters
	defaultCourseSmoothingFactor  = 0.7        // Weight for previous course at high speed
	defaultKalmanProcessNoise     = 0.01       // Tune based on expected acceleration
	defaultKalmanMeasurementNoise = 10.0       // Tune based on GPS accuracy (meters)
	earthRadius                   = 6371000    // meters
)

// GPSFilterConfig holds configuration for the GPS filter
type GPSFilterConfig struct {
	SpeedThreshold         float64 // m/s
	PositionThreshold      float64 // meters
	CourseSmoothingFactor  float64 // Weight for previous course (0-1)
	KalmanProcessNoise     float64 // For Kalman filter
	KalmanMeasurementNoise float64 // For Kalman filter
}

// GPSFilter holds the state and methods for filtering GPS data
type GPSFilter struct {
	config           GPSFilterConfig
	lastValidCourse  float64
	lastValidSpeed   float64
	lastLocation     Location
	isStationary     bool
	kalmanState      *mat.VecDense // [lat, lon, lat_vel, lon_vel]
	kalmanCovariance *mat.Dense    // Covariance matrix
	lastUpdateTime   time.Time
}

// NewGPSFilter creates a new GPSFilter with default or provided config
func NewGPSFilter(cfg *GPSFilterConfig) *GPSFilter {
	filterConfig := GPSFilterConfig{
		SpeedThreshold:         defaultSpeedThreshold,
		PositionThreshold:      defaultPositionThreshold,
		CourseSmoothingFactor:  defaultCourseSmoothingFactor,
		KalmanProcessNoise:     defaultKalmanProcessNoise,
		KalmanMeasurementNoise: defaultKalmanMeasurementNoise,
	}
	if cfg != nil {
		if cfg.SpeedThreshold > 0 {
			filterConfig.SpeedThreshold = cfg.SpeedThreshold
		}
		if cfg.PositionThreshold > 0 {
			filterConfig.PositionThreshold = cfg.PositionThreshold
		}
		if cfg.CourseSmoothingFactor >= 0 && cfg.CourseSmoothingFactor <= 1 {
			filterConfig.CourseSmoothingFactor = cfg.CourseSmoothingFactor
		}
		if cfg.KalmanProcessNoise > 0 {
			filterConfig.KalmanProcessNoise = cfg.KalmanProcessNoise
		}
		if cfg.KalmanMeasurementNoise > 0 {
			filterConfig.KalmanMeasurementNoise = cfg.KalmanMeasurementNoise
		}
	}

	return &GPSFilter{
		config:           filterConfig,
		lastLocation:     Location{Timestamp: time.Now()}, // Initialize with current time
		lastUpdateTime:   time.Now(),
		kalmanState:      mat.NewVecDense(4, nil),
		kalmanCovariance: mat.NewDense(4, 4, nil), // Initialize appropriately
	}
}

// haversineDistance calculates the distance between two lat/lon points
func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	dLat := (lat2 - lat1) * (math.Pi / 180.0)
	dLon := (lon2 - lon1) * (math.Pi / 180.0)

	lat1Rad := lat1 * (math.Pi / 180.0)
	lat2Rad := lat2 * (math.Pi / 180.0)

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Sin(dLon/2)*math.Sin(dLon/2)*math.Cos(lat1Rad)*math.Cos(lat2Rad)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadius * c
}

// FilterLocation applies filtering to the raw GPS location data
func (f *GPSFilter) FilterLocation(rawLoc Location) Location {
	filteredLoc := rawLoc

	// Time difference for Kalman prediction
	dt := rawLoc.Timestamp.Sub(f.lastUpdateTime).Seconds()
	if dt <= 0 { // Ensure dt is positive, or if first update
		dt = 1.0 // Default to 1 second if timestamps are too close or initial
	}

	// Initialize Kalman filter on first valid fix
	if f.kalmanState.AtVec(0) == 0 && f.kalmanState.AtVec(1) == 0 && rawLoc.Latitude != 0 && rawLoc.Longitude != 0 {
		f.kalmanState.SetVec(0, rawLoc.Latitude)
		f.kalmanState.SetVec(1, rawLoc.Longitude)
		// Initial velocities can be zero or derived from first two points if available
		f.kalmanState.SetVec(2, 0) // lat_vel
		f.kalmanState.SetVec(3, 0) // lon_vel
		// Initialize covariance (P) - high uncertainty for initial state
		f.kalmanCovariance.Set(0, 0, f.config.KalmanMeasurementNoise*f.config.KalmanMeasurementNoise)
		f.kalmanCovariance.Set(1, 1, f.config.KalmanMeasurementNoise*f.config.KalmanMeasurementNoise)
		f.kalmanCovariance.Set(2, 2, 1.0) // Velocity uncertainty
		f.kalmanCovariance.Set(3, 3, 1.0) // Velocity uncertainty
		f.lastUpdateTime = rawLoc.Timestamp
		f.lastLocation = rawLoc
		f.lastValidCourse = rawLoc.Course
		f.lastValidSpeed = rawLoc.Speed
		return rawLoc // Return raw on first fix
	}

	// Predict step for Kalman filter
	// State transition matrix (F)
	F := mat.NewDense(4, 4, []float64{
		1, 0, dt, 0,
		0, 1, 0, dt,
		0, 0, 1, 0,
		0, 0, 0, 1,
	})
	// Process noise covariance (Q)
	Q := mat.NewDense(4, 4, []float64{
		0.25 * dt * dt * dt * dt * f.config.KalmanProcessNoise, 0, 0.5 * dt * dt * dt * f.config.KalmanProcessNoise, 0,
		0, 0.25 * dt * dt * dt * dt * f.config.KalmanProcessNoise, 0, 0.5 * dt * dt * dt * f.config.KalmanProcessNoise,
		0.5 * dt * dt * dt * f.config.KalmanProcessNoise, 0, dt * dt * f.config.KalmanProcessNoise, 0,
		0, 0.5 * dt * dt * dt * f.config.KalmanProcessNoise, 0, dt * dt * f.config.KalmanProcessNoise,
	})

	// Predicted state: x_pred = F * x_prev
	xPred := mat.NewVecDense(4, nil)
	xPred.MulVec(F, f.kalmanState)

	// Predicted covariance: P_pred = F * P_prev * F^T + Q
	pPred := mat.NewDense(4, 4, nil)
	pPred.Product(F, f.kalmanCovariance, F.T())
	pPred.Add(pPred, Q)

	// Update step for Kalman filter (if we have a new measurement)
	if rawLoc.Latitude != 0 && rawLoc.Longitude != 0 {
		// Measurement vector (z)
		z := mat.NewVecDense(2, []float64{rawLoc.Latitude, rawLoc.Longitude})

		// Measurement matrix (H)
		H := mat.NewDense(2, 4, []float64{
			1, 0, 0, 0,
			0, 1, 0, 0,
		})

		// Measurement noise covariance (R)
		R := mat.NewDense(2, 2, []float64{
			f.config.KalmanMeasurementNoise * f.config.KalmanMeasurementNoise, 0,
			0, f.config.KalmanMeasurementNoise * f.config.KalmanMeasurementNoise,
		})

		// Innovation or measurement residual: y = z - H * x_pred
		y := mat.NewVecDense(2, nil)
		var hx mat.VecDense
		hx.MulVec(H, xPred)
		y.SubVec(z, &hx)

		// Innovation covariance: S = H * P_pred * H^T + R
		S := mat.NewDense(2, 2, nil)
		var hpPredT mat.Dense
		hpPredT.Product(H, pPred, H.T())
		S.Add(&hpPredT, R)

		// Kalman gain: K = P_pred * H^T * S^-1
		K := mat.NewDense(4, 2, nil)
		var sInv mat.Dense
		err := sInv.Inverse(S)
		if err != nil {
			// If S is singular, skip update or use pseudo-inverse
			// For simplicity, we might just use predicted state if inversion fails
			// This can happen if measurement noise is too small or P_pred becomes singular
			// log.Printf("Kalman S matrix inversion failed: %v", err)
			filteredLoc.Latitude = xPred.AtVec(0)
			filteredLoc.Longitude = xPred.AtVec(1)
		} else {
			var pPredHT mat.Dense
			pPredHT.Mul(pPred, H.T())
			K.Mul(&pPredHT, &sInv)

			// Updated state estimate: x_new = x_pred + K * y
			var ky mat.VecDense
			ky.MulVec(K, y)
			f.kalmanState.AddVec(xPred, &ky)

			// Updated covariance estimate: P_new = (I - K * H) * P_pred
			var kh mat.Dense
			kh.Mul(K, H)
			var ikh mat.Dense
			eye := mat.NewDense(4, 4, nil)
			eye.Product(mat.NewDiagonalRect(4, 4, []float64{1, 1, 1, 1}), mat.NewDiagonalRect(4, 4, []float64{1, 1, 1, 1})) // Create identity matrix
			ikh.Sub(eye, &kh)                                                                                               // This is incorrect, eye should be identity.
			// Correct way to create identity:
			ident4 := mat.NewDense(4, 4, nil)
			for i := 0; i < 4; i++ {
				ident4.Set(i, i, 1)
			}
			ikh.Sub(ident4, &kh)

			f.kalmanCovariance.Mul(&ikh, pPred)

			filteredLoc.Latitude = f.kalmanState.AtVec(0)
			filteredLoc.Longitude = f.kalmanState.AtVec(1)
		}
	} else { // No new measurement, use predicted state
		filteredLoc.Latitude = xPred.AtVec(0)
		filteredLoc.Longitude = xPred.AtVec(1)
		f.kalmanState.CopyVec(xPred)   // Update state to predicted
		f.kalmanCovariance.Copy(pPred) // Update covariance to predicted
	}

	// Stationary detection
	distanceMoved := haversineDistance(f.lastLocation.Latitude, f.lastLocation.Longitude, filteredLoc.Latitude, filteredLoc.Longitude)

	if rawLoc.Speed < f.config.SpeedThreshold && distanceMoved < f.config.PositionThreshold*dt { // dt factor to account for time
		f.isStationary = true
	} else {
		f.isStationary = false
	}

	if f.isStationary {
		filteredLoc.Speed = 0
		filteredLoc.Course = f.lastValidCourse // Keep last known course when stationary
		// Lock position to prevent GPS drift when stationary
		filteredLoc.Latitude = f.lastLocation.Latitude
		filteredLoc.Longitude = f.lastLocation.Longitude
		filteredLoc.Altitude = f.lastLocation.Altitude
		// Reset Kalman velocities to zero to prevent pollution from drift
		f.kalmanState.SetVec(2, 0) // lat_vel
		f.kalmanState.SetVec(3, 0) // lon_vel
		// Update Kalman position state to match locked position
		f.kalmanState.SetVec(0, f.lastLocation.Latitude)
		f.kalmanState.SetVec(1, f.lastLocation.Longitude)
	} else {
		// Use raw GPS speed directly - GPSD reports speed in m/s which is accurate
		// The Kalman filter velocity estimates are too noisy for speed calculation
		filteredLoc.Speed = rawLoc.Speed
		f.lastValidSpeed = filteredLoc.Speed

		// Update course only when moving fast enough for GPS track to be reliable
		// At low speeds, GPS course/track is very noisy due to position uncertainty
		if rawLoc.Speed > f.config.SpeedThreshold {
			newCourse := rawLoc.Course
			if newCourse >= 0 && newCourse < 360 {
				// Calculate speed-proportional smoothing factor
				// At low speeds (1-10 km/h), increase smoothing to dampen GPS noise
				smoothingFactor := f.config.CourseSmoothingFactor
				if rawLoc.Speed < defaultHeadingRampSpeed {
					// Linear interpolation: more smoothing at lower speeds
					// speedRatio goes from 0 (at threshold) to 1 (at ramp speed)
					speedRatio := (rawLoc.Speed - f.config.SpeedThreshold) / (defaultHeadingRampSpeed - f.config.SpeedThreshold)
					// Smoothing goes from 0.95 (low speed) to configured value (high speed)
					smoothingFactor = 0.95 - (0.95-f.config.CourseSmoothingFactor)*speedRatio
				}

				// Smooth course with exponential moving average
				if f.lastValidCourse >= 0 {
					// Handle wrap-around for course (e.g. 350 deg to 10 deg)
					diff := newCourse - f.lastValidCourse
					if diff > 180 {
						diff -= 360
					} else if diff < -180 {
						diff += 360
					}
					smoothedCourse := f.lastValidCourse + (1-smoothingFactor)*diff
					if smoothedCourse < 0 {
						smoothedCourse += 360
					} else if smoothedCourse >= 360 {
						smoothedCourse -= 360
					}
					filteredLoc.Course = smoothedCourse
				} else {
					filteredLoc.Course = newCourse
				}
				f.lastValidCourse = filteredLoc.Course
			}
		} else {
			// At low speeds, keep last valid course
			filteredLoc.Course = f.lastValidCourse
		}
	}

	// Update last known good location and time
	f.lastLocation = filteredLoc        // Store the filtered location
	f.lastUpdateTime = rawLoc.Timestamp // Use raw timestamp for dt calculation next cycle

	return filteredLoc
}

// Helper to initialize a matrix with an error check
func mustNewDense(r, c int, data []float64) *mat.Dense {
	m := mat.NewDense(r, c, data)
	if m == nil {
		panic(errors.Errorf("failed to create matrix %dx%d", r, c))
	}
	return m
}
