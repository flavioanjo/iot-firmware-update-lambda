@echo off
echo ==================================================
echo    Starting Lambda in local mode...
echo ==================================================

set LOCAL_ENVIRONMENT=true
set DB_CONNECTION_STRING=Server=your-rds-instance.region.rds.amazonaws.com;Database=your_database_name;User Id=your_db_user;Password=your_db_password

go run main.go

pause
