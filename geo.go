package redimo

import (
	"context"
	"fmt"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/golang/geo/s1"
	"github.com/golang/geo/s2"
	"github.com/mmcloughlin/geohash"
)

const EarthRadiusMeters = 6372797.560856

type Location struct {
	Lat float64
	Lon float64
}

func (l Location) s2CellID() string {
	return fmt.Sprintf("%d", s2.CellIDFromLatLng(l.s2LatLng()))
}

func (l Location) Geohash() string {
	return geohash.Encode(l.Lat, l.Lon)
}

func (l Location) toAV() dynamodb.AttributeValue {
	return dynamodb.AttributeValue{
		N: aws.String(l.s2CellID()),
	}
}

func (l *Location) setCellIDString(cellIDStr string) {
	cellID, _ := strconv.ParseUint(cellIDStr, 10, 64)
	s2Cell := s2.CellID(cellID)
	s2LatLon := s2Cell.LatLng()
	l.Lat = s2LatLon.Lat.Degrees()
	l.Lon = s2LatLon.Lng.Degrees()
}

func (l Location) DistanceTo(other Location, unit Unit) float64 {
	return Meters.To(unit, l.s2LatLng().Distance(other.s2LatLng()).Radians()*EarthRadiusMeters)
}

func (l Location) s2LatLng() s2.LatLng {
	return s2.LatLngFromDegrees(l.Lat, l.Lon)
}

func fromCellIDString(cellID string) (l Location) {
	l.setCellIDString(cellID)
	return
}

type Unit float64

func (from Unit) To(to Unit, d float64) float64 {
	return (d / float64(to)) * float64(from)
}

const (
	Meters     Unit = 1.0
	Kilometers Unit = 1000.0
	Miles      Unit = 1609.34
	Feet       Unit = 0.3048
)

func (c Client) GEOADD(key string, members map[string]Location) (addedCount int64, err error) {
	for member, location := range members {
		builder := newExpresionBuilder()
		builder.updateSetAV(skGeoCell, location.toAV())

		_, err = c.ddbClient.UpdateItemRequest(&dynamodb.UpdateItemInput{
			ConditionExpression:       builder.conditionExpression(),
			ExpressionAttributeNames:  builder.expressionAttributeNames(),
			ExpressionAttributeValues: builder.expressionAttributeValues(),
			Key:                       keyDef{pk: key, sk: member}.toAV(),
			TableName:                 aws.String(c.table),
			UpdateExpression:          builder.updateExpression(),
		}).Send(context.TODO())

		if err != nil {
			return addedCount, err
		}
	}

	return addedCount, nil
}

func (c Client) GEODIST(key string, member1, member2 string, unit Unit) (distance float64, ok bool, err error) {
	locations, err := c.GEOPOS(key, member1, member2)
	if err != nil || len(locations) < 2 {
		return
	}

	return locations[member1].DistanceTo(locations[member2], unit), true, nil
}

func (c Client) GEOHASH(key string, members ...string) (geohashes []string, err error) {
	locations, err := c.GEOPOS(key, members...)

	for _, member := range members {
		if location, found := locations[member]; found {
			geohashes = append(geohashes, location.Geohash())
		} else {
			geohashes = append(geohashes, "")
		}
	}

	return
}

func (c Client) GEOPOS(key string, members ...string) (locations map[string]Location, err error) {
	locations = make(map[string]Location)

	for _, member := range members {
		resp, err := c.ddbClient.GetItemRequest(&dynamodb.GetItemInput{
			ConsistentRead: aws.Bool(c.consistentReads),
			Key:            keyDef{pk: key, sk: member}.toAV(),
			TableName:      aws.String(c.table),
		}).Send(context.TODO())

		if err != nil {
			return locations, err
		}

		if len(resp.Item) > 0 {
			locations[member] = fromCellIDString(aws.StringValue(resp.Item[skGeoCell].N))
		}
	}

	return
}

func (c Client) GEORADIUS(key string, center Location, radius float64, radiusUnit Unit, count int64) (positions map[string]Location, err error) {
	positions = make(map[string]Location)
	radiusCap := s2.CapFromCenterAngle(s2.PointFromLatLng(center.s2LatLng()), s1.Angle(radiusUnit.To(Meters, radius)/EarthRadiusMeters))

	for _, cellID := range radiusCap.CellUnionBound() {
		builder := newExpresionBuilder()
		builder.addConditionEquality(pk, StringValue{key})
		builder.condition(fmt.Sprintf("#%v BETWEEN :start AND :stop", skGeoCell), skGeoCell)
		builder.values["start"] = dynamodb.AttributeValue{N: aws.String(fmt.Sprintf("%d", cellID.RangeMin()))}
		builder.values["stop"] = dynamodb.AttributeValue{N: aws.String(fmt.Sprintf("%d", cellID.RangeMax()))}

		var cursor map[string]dynamodb.AttributeValue

		hasMoreResults := true

		for hasMoreResults && count > 0 {
			resp, err := c.ddbClient.QueryRequest(&dynamodb.QueryInput{
				ConsistentRead:            aws.Bool(c.consistentReads),
				ExclusiveStartKey:         cursor,
				ExpressionAttributeNames:  builder.expressionAttributeNames(),
				ExpressionAttributeValues: builder.expressionAttributeValues(),
				IndexName:                 c.getIndex(skGeoCell),
				KeyConditionExpression:    builder.conditionExpression(),
				Limit:                     aws.Int64(count),
				TableName:                 aws.String(c.table),
			}).Send(context.TODO())
			if err != nil {
				return positions, err
			}

			if len(resp.LastEvaluatedKey) > 0 {
				cursor = resp.LastEvaluatedKey
			} else {
				hasMoreResults = false
			}

			for _, item := range resp.Items {
				location := fromCellIDString(aws.StringValue(item[skGeoCell].N))
				member := aws.StringValue(item[sk].S)

				if center.DistanceTo(location, radiusUnit) <= radius {
					positions[member] = location
					count--
				}
			}
		}
	}

	return
}

func (c Client) GEORADIUSBYMEMBER(key string, member string, radius float64, radiusUnit Unit, count int64) (positions map[string]Location, err error) {
	locations, err := c.GEOPOS(key, member)
	if err == nil {
		positions, err = c.GEORADIUS(key, locations[member], radius, radiusUnit, count)
	}

	return
}